/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package ghcache implements an HTTP cache optimized for caching responses
// from the GitHub API (https://api.github.com).
//
// Specifically, it enforces a cache policy that revalidates every cache hit
// with a conditional request to upstream regardless of cache entry freshness
// because conditional requests for unchanged resources don't cost any API
// tokens!!! See: https://developer.github.com/v3/#conditional-requests
//
// It also provides request coalescing and prometheus instrumentation.
package ghcache

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"github.com/gomodule/redigo/redis"
	"github.com/gregjones/httpcache"
	"github.com/gregjones/httpcache/diskcache"
	rediscache "github.com/gregjones/httpcache/redis"
	"github.com/peterbourgon/diskv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
	"k8s.io/test-infra/ghproxy/ghmetrics"
)

type CacheResponseMode string

// Cache response modes describe how ghcache fulfilled a request.
const (
	CacheModeHeader = "X-Cache-Mode"

	ModeError   CacheResponseMode = "ERROR"    // internal error handling request
	ModeNoStore CacheResponseMode = "NO-STORE" // response not cacheable
	ModeMiss    CacheResponseMode = "MISS"     // not in cache, request proxied and response cached.
	ModeChanged CacheResponseMode = "CHANGED"  // cache value invalid: resource changed, cache updated
	// The modes below are the happy cases in which the request is fulfilled for
	// free (no API tokens used).
	ModeCoalesced   CacheResponseMode = "COALESCED"   // coalesced request, this is a copied response
	ModeRevalidated CacheResponseMode = "REVALIDATED" // cached value revalidated and returned

	// cacheEntryCreationDateHeader contains the creation date of the cache entry
	cacheEntryCreationDateHeader = "X-PROW-REQUEST-DATE"

	// TokenBudgetIdentifierHeader is used to identify the token budget for
	// which metrics should be recorded if set. If unset, the sha256sum of
	// the Authorization header will be used.
	TokenBudgetIdentifierHeader = "X-PROW-GHCACHE-TOKEN-BUDGET-IDENTIFIER"

	// TokenExpiryAtHeader includes a date at which the passed token expires and all associated caches
	// can be cleaned up. It's value must be in RFC3339 format.
	TokenExpiryAtHeader = "X-PROW-TOKEN-EXPIRES-AT"
)

func CacheModeIsFree(mode CacheResponseMode) bool {
	switch mode {
	case ModeCoalesced:
		return true
	case ModeRevalidated:
		return true
	case ModeError:
		// In this case we did not successfully communicate with the GH API, so no
		// token is used, but we also don't return a response, so ModeError won't
		// ever be returned as a value of CacheModeHeader.
		return true
	}
	return false
}

// outboundConcurrencyGauge provides the 'concurrent_outbound_requests' gauge that
// is global to the proxy.
var outboundConcurrencyGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "concurrent_outbound_requests",
	Help: "How many concurrent requests are in flight to GitHub servers.",
})

// pendingOutboundConnectionsGauge provides the 'pending_outbound_requests' gauge that
// is global to the proxy.
var pendingOutboundConnectionsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "pending_outbound_requests",
	Help: "How many pending requests are waiting to be sent to GitHub servers.",
})

var cachePartitionsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "ghcache_cache_parititions",
		Help: "Which cache partitions exist",
	},
	[]string{"token_hash"},
)

func init() {

	prometheus.MustRegister(outboundConcurrencyGauge)
	prometheus.MustRegister(pendingOutboundConnectionsGauge)
	prometheus.MustRegister(cachePartitionsCounter)
}

func cacheResponseMode(headers http.Header) CacheResponseMode {
	if strings.Contains(headers.Get("Cache-Control"), "no-store") {
		return ModeNoStore
	}
	if strings.Contains(headers.Get("Status"), "304 Not Modified") {
		return ModeRevalidated
	}
	if headers.Get("X-Conditional-Request") != "" {
		return ModeChanged
	}
	return ModeMiss
}

func newThrottlingTransport(maxConcurrency int, delegate http.RoundTripper) http.RoundTripper {
	return &throttlingTransport{sem: semaphore.NewWeighted(int64(maxConcurrency)), delegate: delegate}
}

// throttlingTransport throttles outbound concurrency from the proxy
type throttlingTransport struct {
	sem      *semaphore.Weighted
	delegate http.RoundTripper
}

func (c *throttlingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	pendingOutboundConnectionsGauge.Inc()
	if err := c.sem.Acquire(context.Background(), 1); err != nil {
		logrus.WithField("cache-key", req.URL.String()).WithError(err).Error("Internal error acquiring semaphore.")
		return nil, err
	}
	defer c.sem.Release(1)
	pendingOutboundConnectionsGauge.Dec()
	outboundConcurrencyGauge.Inc()
	defer outboundConcurrencyGauge.Dec()
	return c.delegate.RoundTrip(req)
}

// upstreamTransport changes response headers from upstream before they
// reach the cache layer in order to force the caching policy we require.
//
// By default github responds to PR requests with:
//    Cache-Control: private, max-age=60, s-maxage=60
// Which means the httpcache would not consider anything stale for 60 seconds.
// However, we want to always revalidate cache entries using ETags and last
// modified times so this RoundTripper overrides response headers to:
//    Cache-Control: no-cache
// This instructs the cache to store the response, but always consider it stale.
type upstreamTransport struct {
	delegate http.RoundTripper
	hasher   ghmetrics.Hasher
}

func (u upstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	etag := req.Header.Get("if-none-match")
	var tokenBudgetName string
	if val := req.Header.Get(TokenBudgetIdentifierHeader); val != "" {
		tokenBudgetName = val
	} else {
		tokenBudgetName = u.hasher.Hash(req)
	}

	reqStartTime := time.Now()
	// Don't modify request, just pass to delegate.
	resp, err := u.delegate.RoundTrip(req)
	if err != nil {
		ghmetrics.CollectRequestTimeoutMetrics(tokenBudgetName, req.URL.Path, req.Header.Get("User-Agent"), reqStartTime, time.Now())
		logrus.WithField("cache-key", req.URL.String()).WithError(err).Warn("Error from upstream (GitHub).")
		return nil, err
	}
	responseTime := time.Now()
	roundTripTime := responseTime.Sub(reqStartTime)

	if resp.StatusCode >= 400 {
		// Don't store errors. They can't be revalidated to save API tokens.
		resp.Header.Set("Cache-Control", "no-store")
	} else {
		resp.Header.Set("Cache-Control", "no-cache")
		if resp.StatusCode != http.StatusNotModified {
			// Used for metrics about the age of cached requests
			resp.Header.Set(cacheEntryCreationDateHeader, strconv.Itoa(int(time.Now().Unix())))
		}
	}
	if etag != "" {
		resp.Header.Set("X-Conditional-Request", etag)
	}

	apiVersion := "v3"
	if strings.HasPrefix(req.URL.Path, "graphql") || strings.HasPrefix(req.URL.Path, "/graphql") {
		resp.Header.Set("Cache-Control", "no-store")
		apiVersion = "v4"
	}

	ghmetrics.CollectGitHubTokenMetrics(tokenBudgetName, apiVersion, resp.Header, reqStartTime, responseTime)
	ghmetrics.CollectGitHubRequestMetrics(tokenBudgetName, req.URL.Path, strconv.Itoa(resp.StatusCode), req.Header.Get("User-Agent"), roundTripTime.Seconds())

	return resp, nil
}

const LogMessageWithDiskPartitionFields = "Not using a partitioned cache because legacyDisablePartitioningByAuthHeader is true"

// NewDiskCache creates a GitHub cache RoundTripper that is backed by a disk
// cache.
// It supports a partitioned cache.
func NewDiskCache(delegate http.RoundTripper, cacheDir string, cacheSizeGB, maxConcurrency int, legacyDisablePartitioningByAuthHeader bool, cachePruneInterval time.Duration) http.RoundTripper {
	if legacyDisablePartitioningByAuthHeader {
		diskCache := diskcache.NewWithDiskv(
			diskv.New(diskv.Options{
				BasePath:     path.Join(cacheDir, "data"),
				TempDir:      path.Join(cacheDir, "temp"),
				CacheSizeMax: uint64(cacheSizeGB) * uint64(1000000000), // convert G to B
			}))
		return NewFromCache(delegate,
			func(partitionKey string, _ *time.Time) httpcache.Cache {
				logrus.WithField("cache-base-path", path.Join(cacheDir, "data", partitionKey)).
					WithField("cache-temp-path", path.Join(cacheDir, "temp", partitionKey)).
					Warning(LogMessageWithDiskPartitionFields)
				return diskCache
			},
			maxConcurrency,
		)
	}

	go func() {
		for range time.NewTicker(cachePruneInterval).C {
			Prune(cacheDir, time.Now)
		}
	}()
	return NewFromCache(delegate,
		func(partitionKey string, expiresAt *time.Time) httpcache.Cache {
			basePath := path.Join(cacheDir, "data", partitionKey)
			tempDir := path.Join(cacheDir, "temp", partitionKey)
			if err := writecachePartitionMetadata(basePath, tempDir, expiresAt); err != nil {
				logrus.WithError(err).Warn("Failed to write cache metadata file, pruning will not work")
			}
			return diskcache.NewWithDiskv(
				diskv.New(diskv.Options{
					BasePath:     basePath,
					TempDir:      tempDir,
					CacheSizeMax: uint64(cacheSizeGB) * uint64(1000000000), // convert G to B
				}))
		},
		maxConcurrency,
	)
}

func Prune(baseDir string, now func() time.Time) {
	// All of this would be easier if the structure was base/partition/{data,temp}
	// but because of compatibility we can not change it.
	for _, dir := range []string{"data", "temp"} {
		base := path.Join(baseDir, dir)
		cachePartitionCandidates, err := os.ReadDir(base)
		if err != nil {
			logrus.WithError(err).Warn("os.ReadDir failed")
			// no continue, os.ReadDir returns partial results if it encounters an error
		}
		for _, cachePartitionCandidate := range cachePartitionCandidates {
			if !cachePartitionCandidate.IsDir() {
				continue
			}
			metadataPath := path.Join(base, cachePartitionCandidate.Name(), cachePartitionMetadataFileName)

			// Read optimistically and just ignore errors
			raw, err := ioutil.ReadFile(metadataPath)
			if err != nil {
				continue
			}
			var metadata cachePartitionMetadata
			if err := json.Unmarshal(raw, &metadata); err != nil {
				logrus.WithError(err).WithField("filepath", metadataPath).Error("failed to deserialize metadata file")
				continue
			}
			if metadata.ExpiresAt.After(now()) {
				continue
			}
			paritionPath := filepath.Dir(metadataPath)
			logrus.WithField("path", paritionPath).WithField("expiresAt", metadata.ExpiresAt.String()).Info("Cleaning up expired cache parition")
			if err := os.RemoveAll(paritionPath); err != nil {
				logrus.WithError(err).WithField("path", paritionPath).Error("failed to delete expired cache parition")
			}
		}
	}
}

func writecachePartitionMetadata(basePath, tempDir string, expiresAt *time.Time) error {
	// No expiry header for the token was passed, likely it is a PAT which never expires.
	if expiresAt == nil {
		return nil
	}
	metadata := cachePartitionMetadata{ExpiresAt: metav1.Time{Time: *expiresAt}}
	serialized, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("failed to serialize: %w", err)
	}

	var errs []error
	for _, destBase := range []string{basePath, tempDir} {
		if err := os.MkdirAll(destBase, 0755); err != nil {
			errs = append(errs, fmt.Errorf("failed to create dir %s: %w", destBase, err))
		}
		dest := path.Join(destBase, cachePartitionMetadataFileName)
		if err := ioutil.WriteFile(dest, serialized, 0644); err != nil {
			errs = append(errs, fmt.Errorf("failed to write %s: %w", dest, err))
		}
	}

	return utilerrors.NewAggregate(errs)
}

const cachePartitionMetadataFileName = ".cache_metadata.json"

type cachePartitionMetadata struct {
	ExpiresAt metav1.Time `json:"expires_at"`
}

// NewMemCache creates a GitHub cache RoundTripper that is backed by a memory
// cache.
// It supports a partitioned cache.
func NewMemCache(delegate http.RoundTripper, maxConcurrency int) http.RoundTripper {
	return NewFromCache(delegate,
		func(_ string, _ *time.Time) httpcache.Cache { return httpcache.NewMemoryCache() },
		maxConcurrency)
}

// CachePartitionCreator creates a new cache partition using the given key
type CachePartitionCreator func(partitionKey string, expiresAt *time.Time) httpcache.Cache

// NewFromCache creates a GitHub cache RoundTripper that is backed by the
// specified httpcache.Cache implementation.
func NewFromCache(delegate http.RoundTripper, cache CachePartitionCreator, maxConcurrency int) http.RoundTripper {
	hasher := ghmetrics.NewCachingHasher()
	return newPartitioningRoundTripper(func(partitionKey string, expiresAt *time.Time) http.RoundTripper {
		cacheTransport := httpcache.NewTransport(cache(partitionKey, expiresAt))
		cacheTransport.Transport = newThrottlingTransport(maxConcurrency, upstreamTransport{delegate: delegate, hasher: hasher})
		return &requestCoalescer{
			keys:     make(map[string]*responseWaiter),
			delegate: cacheTransport,
			hasher:   hasher,
		}
	})
}

// NewRedisCache creates a GitHub cache RoundTripper that is backed by a Redis
// cache.
// Important note: The redis implementation does not support partitioning the cache
// which means that requests to the same path from different tokens will invalidate
// each other.
func NewRedisCache(delegate http.RoundTripper, redisAddress string, maxConcurrency int) http.RoundTripper {
	conn, err := redis.Dial("tcp", redisAddress)
	if err != nil {
		logrus.WithError(err).Fatal("Error connecting to Redis")
	}
	redisCache := rediscache.NewWithClient(conn)
	return NewFromCache(delegate,
		func(_ string, _ *time.Time) httpcache.Cache { return redisCache },
		maxConcurrency)
}
