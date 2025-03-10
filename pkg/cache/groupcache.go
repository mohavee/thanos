// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package cache

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/route"
	"github.com/thanos-io/thanos/pkg/discovery/dns"
	"github.com/thanos-io/thanos/pkg/extprom"
	"github.com/thanos-io/thanos/pkg/model"
	"github.com/thanos-io/thanos/pkg/objstore"
	"github.com/thanos-io/thanos/pkg/runutil"
	"github.com/thanos-io/thanos/pkg/store/cache/cachekey"
	"github.com/vimeo/galaxycache"
	galaxyhttp "github.com/vimeo/galaxycache/http"
	"gopkg.in/yaml.v2"
)

type Groupcache struct {
	galaxy   *galaxycache.Galaxy
	universe *galaxycache.Universe
	logger   log.Logger
}

// GroupcacheConfig holds the in-memory cache config.
type GroupcacheConfig struct {
	// Addresses of statically configured peers (repeatable). The scheme may be prefixed with 'dns+' or 'dnssrv+' to detect store API servers through respective DNS lookups.
	// Typically, you'd want something like `dns+http://thanos-store:42/`.
	Peers []string `yaml:"peers"`

	// Address of ourselves in the peer list. This needs to be set to `http://external-ip:HTTP_PORT`
	// of the current instance.
	SelfURL string `yaml:"self_url"`

	// Maximum size of the hot in-memory cache.
	MaxSize model.Bytes `yaml:"max_size"`

	// Group's name. All of the instances need to be using the same group and point to the same bucket.
	GroupcacheGroup string `yaml:"groupcache_group"`

	// DNS SD resolver to use.
	DNSSDResolver dns.ResolverType `yaml:"dns_sd_resolver"`

	// How often we should resolve the addresses.
	DNSInterval time.Duration `yaml:"dns_interval"`
}

var (
	DefaultGroupcacheConfig = GroupcacheConfig{
		MaxSize:       250 * 1024 * 1024,
		DNSSDResolver: dns.GolangResolverType,
		DNSInterval:   1 * time.Minute,
	}
)

// parseGroupcacheConfig unmarshals a buffer into a GroupcacheConfig with default values.
func parseGroupcacheConfig(conf []byte) (GroupcacheConfig, error) {
	config := DefaultGroupcacheConfig
	if err := yaml.Unmarshal(conf, &config); err != nil {
		return GroupcacheConfig{}, err
	}

	if len(config.Peers) == 0 {
		config.Peers = append(config.Peers, config.SelfURL)
	}

	return config, nil
}

// NewGroupcache creates a new Groupcache instance.
func NewGroupcache(logger log.Logger, reg prometheus.Registerer, conf []byte, basepath string, r *route.Router, bucket objstore.Bucket, cfg *CachingBucketConfig) (*Groupcache, error) {
	config, err := parseGroupcacheConfig(conf)
	if err != nil {
		return nil, err
	}

	return NewGroupcacheWithConfig(logger, reg, config, basepath, r, bucket, cfg)
}

// NewGroupcacheWithConfig creates a new Groupcache instance with the given config.
func NewGroupcacheWithConfig(logger log.Logger, reg prometheus.Registerer, conf GroupcacheConfig, basepath string, r *route.Router, bucket objstore.Bucket,
	cfg *CachingBucketConfig) (*Groupcache, error) {
	httpProto := galaxyhttp.NewHTTPFetchProtocol(&galaxyhttp.HTTPOptions{
		BasePath: basepath,
	})
	universe := galaxycache.NewUniverse(httpProto, conf.SelfURL)

	dnsGroupcacheProvider := dns.NewProvider(
		logger,
		extprom.WrapRegistererWithPrefix("thanos_store_groupcache_", reg),
		dns.ResolverType(conf.DNSSDResolver),
	)
	ticker := time.NewTicker(conf.DNSInterval)

	go func() {
		for {
			if err := dnsGroupcacheProvider.Resolve(context.Background(), conf.Peers); err != nil {
				level.Error(logger).Log("msg", "failed to resolve addresses for groupcache", "err", err)
			} else {
				err := universe.Set(dnsGroupcacheProvider.Addresses()...)
				if err != nil {
					level.Error(logger).Log("msg", "failed to set peers for groupcache", "err", err)
				}
			}

			<-ticker.C
		}
	}()

	mux := http.NewServeMux()
	galaxyhttp.RegisterHTTPHandler(universe, &galaxyhttp.HTTPOptions{
		BasePath: basepath,
	}, mux)
	r.Get(basepath, mux.ServeHTTP)

	galaxy := universe.NewGalaxy(conf.GroupcacheGroup, int64(conf.MaxSize), galaxycache.GetterFunc(
		func(ctx context.Context, id string, dest galaxycache.Codec) error {
			parsedData, err := cachekey.ParseBucketCacheKey(id)
			if err != nil {
				return err
			}

			switch parsedData.Verb {
			case cachekey.AttributesVerb:
				_, attrCfg := cfg.FindAttributesConfig(parsedData.Name)
				if attrCfg == nil {
					panic("caching bucket layer must not call on unconfigured paths")
				}

				attrs, err := bucket.Attributes(ctx, parsedData.Name)
				if err != nil {
					return err
				}

				finalAttrs, err := json.Marshal(attrs)
				if err != nil {
					return err
				}

				return dest.UnmarshalBinary(finalAttrs, time.Now().Add(attrCfg.TTL))
			case cachekey.IterVerb:
				_, iterCfg := cfg.FindIterConfig(parsedData.Name)
				if iterCfg == nil {
					panic("caching bucket layer must not call on unconfigured paths")
				}

				var list []string
				if err := bucket.Iter(ctx, parsedData.Name, func(s string) error {
					list = append(list, s)
					return nil
				}); err != nil {
					return err
				}

				encodedList, err := json.Marshal(list)
				if err != nil {
					return err
				}

				return dest.UnmarshalBinary(encodedList, time.Now().Add(iterCfg.TTL))
			case cachekey.ContentVerb:
				_, contentCfg := cfg.FindGetConfig(parsedData.Name)
				if contentCfg == nil {
					panic("caching bucket layer must not call on unconfigured paths")
				}
				rc, err := bucket.Get(ctx, parsedData.Name)
				if err != nil {
					return err
				}
				defer runutil.CloseWithLogOnErr(logger, rc, "closing get")

				b, err := ioutil.ReadAll(rc)
				if err != nil {
					return err
				}

				return dest.UnmarshalBinary(b, time.Now().Add(contentCfg.ContentTTL))
			case cachekey.ExistsVerb:
				_, existsCfg := cfg.FindExistConfig(parsedData.Name)
				if existsCfg == nil {
					panic("caching bucket layer must not call on unconfigured paths")
				}
				exists, err := bucket.Exists(ctx, parsedData.Name)
				if err != nil {
					return err
				}

				if exists {
					return dest.UnmarshalBinary([]byte(strconv.FormatBool(exists)), time.Now().Add(existsCfg.ExistsTTL))
				} else {
					return dest.UnmarshalBinary([]byte(strconv.FormatBool(exists)), time.Now().Add(existsCfg.DoesntExistTTL))
				}

			case cachekey.SubrangeVerb:
				_, subrangeCfg := cfg.FindGetRangeConfig(parsedData.Name)
				if subrangeCfg == nil {
					panic("caching bucket layer must not call on unconfigured paths")
				}
				rc, err := bucket.GetRange(ctx, parsedData.Name, parsedData.Start, parsedData.End-parsedData.Start)
				if err != nil {
					return err
				}
				defer runutil.CloseWithLogOnErr(logger, rc, "closing get_range")

				b, err := ioutil.ReadAll(rc)
				if err != nil {
					return err
				}

				return dest.UnmarshalBinary(b, time.Now().Add(subrangeCfg.SubrangeTTL))

			}

			return nil
		},
	))

	RegisterCacheStatsCollector(galaxy, reg)

	return &Groupcache{
		logger:   logger,
		galaxy:   galaxy,
		universe: universe,
	}, nil
}

func (c *Groupcache) Store(ctx context.Context, data map[string][]byte, ttl time.Duration) {
	// Noop since cache is already filled during fetching.
}

func (c *Groupcache) Fetch(ctx context.Context, keys []string) map[string][]byte {
	data := map[string][]byte{}

	for _, k := range keys {
		codec := galaxycache.ByteCodec{}

		if err := c.galaxy.Get(ctx, k, &codec); err != nil {
			level.Error(c.logger).Log("msg", "failed fetching data from groupcache", "err", err, "key", k)
			continue
		}

		retrievedData, _, err := codec.MarshalBinary()
		if err != nil {
			level.Error(c.logger).Log("msg", "failed retrieving data", "err", err, "key", k)
			continue
		}

		if len(retrievedData) > 0 {
			data[k] = retrievedData
		}
	}

	return data
}

func (c *Groupcache) Name() string {
	return c.galaxy.Name()
}

type CacheStatsCollector struct {
	galaxy *galaxycache.Galaxy

	// GalaxyCache Metric descriptions.
	gets              *prometheus.Desc
	loads             *prometheus.Desc
	peerLoads         *prometheus.Desc
	peerLoadErrors    *prometheus.Desc
	backendLoads      *prometheus.Desc
	backendLoadErrors *prometheus.Desc
	cacheHits         *prometheus.Desc
}

// RegisterCacheStatsCollector registers a groupcache metrics collector.
func RegisterCacheStatsCollector(galaxy *galaxycache.Galaxy, reg prometheus.Registerer) {
	gets := prometheus.NewDesc("thanos_cache_groupcache_get_requests_total", "Total number of get requests, including from peers.", nil, nil)
	loads := prometheus.NewDesc("thanos_cache_groupcache_loads_total", "Total number of loads from backend (gets - cacheHits).", nil, nil)
	peerLoads := prometheus.NewDesc("thanos_cache_groupcache_peer_loads_total", "Total number of loads from peers (remote load or remote cache hit).", nil, nil)
	peerLoadErrors := prometheus.NewDesc("thanos_cache_groupcache_peer_load_errors_total", "Total number of errors from peer loads.", nil, nil)
	backendLoads := prometheus.NewDesc("thanos_cache_groupcache_backend_loads_total", "Total number of direct backend loads.", nil, nil)
	backendLoadErrors := prometheus.NewDesc("thanos_cache_groupcache_backend_load_errors_total", "Total number of errors on direct backend loads.", nil, nil)
	cacheHits := prometheus.NewDesc("thanos_cache_groupcache_hits_total", "Total number of cache hits.", []string{"type"}, nil)

	collector := &CacheStatsCollector{
		galaxy:            galaxy,
		gets:              gets,
		loads:             loads,
		peerLoads:         peerLoads,
		peerLoadErrors:    peerLoadErrors,
		backendLoads:      backendLoads,
		backendLoadErrors: backendLoadErrors,
		cacheHits:         cacheHits,
	}
	reg.MustRegister(collector)
}

func (s *CacheStatsCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(s.gets, prometheus.CounterValue, float64(s.galaxy.Stats.Gets.Get()))
	ch <- prometheus.MustNewConstMetric(s.loads, prometheus.CounterValue, float64(s.galaxy.Stats.Loads.Get()))
	ch <- prometheus.MustNewConstMetric(s.peerLoads, prometheus.CounterValue, float64(s.galaxy.Stats.PeerLoads.Get()))
	ch <- prometheus.MustNewConstMetric(s.peerLoadErrors, prometheus.CounterValue, float64(s.galaxy.Stats.PeerLoadErrors.Get()))
	ch <- prometheus.MustNewConstMetric(s.backendLoads, prometheus.CounterValue, float64(s.galaxy.Stats.BackendLoads.Get()))
	ch <- prometheus.MustNewConstMetric(s.backendLoadErrors, prometheus.CounterValue, float64(s.galaxy.Stats.BackendLoadErrors.Get()))
	ch <- prometheus.MustNewConstMetric(s.cacheHits, prometheus.CounterValue, float64(s.galaxy.Stats.MaincacheHits.Get()), galaxycache.MainCache.String())
	ch <- prometheus.MustNewConstMetric(s.cacheHits, prometheus.CounterValue, float64(s.galaxy.Stats.HotcacheHits.Get()), galaxycache.HotCache.String())
}

func (s *CacheStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(s, ch)
}
