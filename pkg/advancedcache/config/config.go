package config

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"time"
)

const (
	Prod = "prod"
	Dev  = "dev"
	Test = "test"
)

type TraefikIntermediateConfig struct {
	ConfigPath string `yaml:"configPath" mapstructure:"configPath"`
}

type Cache struct {
	Cache CacheBox `yaml:"cache"`
}

func (c *Cache) IsProd() bool {
	return c.Cache.Env == Prod
}

func (c *Cache) IsDev() bool {
	return c.Cache.Env == Dev
}

func (c *Cache) IsTest() bool {
	return c.Cache.Env == Test
}

type Env struct {
	Value string `yaml:"value"`
}

type CacheBox struct {
	Env         string           `yaml:"env"`
	Enabled     bool             `yaml:"enabled"`
	Logs        Logs             `yaml:"logs"`
	ForceGC     ForceGC          `yaml:"forceGC"`
	LifeTime    Lifetime         `yaml:"lifetime"`
	Upstream    Upstream         `yaml:"upstream"`
	Persistence Persistence      `yaml:"persistence"`
	Preallocate Preallocation    `yaml:"preallocate"`
	Eviction    Eviction         `yaml:"eviction"`
	Refresh     Refresh          `yaml:"refresh"`
	Storage     Storage          `yaml:"storage"`
	Rules       map[string]*Rule `yaml:"rules"`
}

type ForceGC struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
}

type Logs struct {
	Level string `yaml:"level"` // Any zerolog.Level.
	Stats bool   `yaml:"stats"` // Should the statistic like num evictions, refreshes, rps, memory usage and so on be written in /std/out?
}

type Lifetime struct {
	MaxReqDuration                  time.Duration `yaml:"maxReqDur"`             // If a request lifetime is longer than 100ms then request will be canceled by context.
	EscapeMaxReqDurationHeader      string        `yaml:"escapeMaxReqDurHeader"` // If the header exists the timeout above will be skipped.
	EscapeMaxReqDurationHeaderBytes []byte        // The same value but converted into slice bytes.
}

type Upstream struct {
	Url     string        `yaml:"url"`     // Reverse Proxy url (can be found in Caddyfile). URL to underlying backend.
	Rate    int           `yaml:"rate"`    // Rate limiting reqs to backend per second.
	Timeout time.Duration `yaml:"timeout"` // Timeout for requests to backend.
}

type Dump struct {
	IsEnabled   bool   `yaml:"enabled"`
	Dir         string `yaml:"dumpDir"`
	Name        string `yaml:"dumpName"`
	MaxVersions int    `yaml:"maxVersions"`
}

type Persistence struct {
	Dump Dump `yaml:"dump"`
}

type Preallocation struct {
	PerShard int `yaml:"perShard"`
}

type Eviction struct {
	Policy    string  `yaml:"policy"`    // at now, it's only "lru" + TinyLFU
	Threshold float64 `yaml:"threshold"` // 0.9 means 90%
}

type Storage struct {
	Type string `yaml:"type"` // "malloc"
	Size uint   `yaml:"size"` // 21474836480=2gb(bytes)
}

type Refresh struct {
	Enabled bool `yaml:"enabled"`
	// TTL - refresh TTL (max time life of response item in cache without refreshing).
	TTL time.Duration `yaml:"ttl"` // e.g. "1d" (responses with 200 status code)
	// ErrorTTL - error refresh TTL (max time life of response item with non 200 status code in cache without refreshing).
	ErrorTTL time.Duration `yaml:"errorTTL"` // e.g. "1h" (responses with non 200 status code)
	Rate     int           `yaml:"rate"`     // Rate limiting to external backend.
	ScanRate int           `yaml:"scanRate"` // Rate limiting of num scans items per second.
	// beta определяет коэффициент, используемый для вычисления случайного момента обновления кэша.
	// Чем выше beta, тем чаще кэш будет обновляться до истечения TTL.
	// Формула взята из подхода "stochastic cache expiration" (см. Google Staleness paper):
	// expireTime = ttl * (-beta * ln(random()))
	// Подробнее: RFC 5861 и https://web.archive.org/web/20100829170210/http://labs.google.com/papers/staleness.pdf
	// beta: "0.4"
	Beta     float64       `yaml:"beta"` // between 0 and 1
	MinStale time.Duration // computed=time.Duration(float64(TTL/ErrorTTL) * Beta)
}

type Gzip struct {
	Enabled   bool `yaml:"enabled"`
	Threshold int  `yaml:"threshold"`
}

type Rule struct {
	Gzip       Gzip `yaml:"gzip"`
	PathBytes  []byte
	TTL        time.Duration `yaml:"ttl"`      // TTL for this rule.
	ErrorTTL   time.Duration `yaml:"errorTTL"` // ErrorTTL for this rule.
	Beta       float64       `yaml:"beta"`     // between 0 and 1
	CacheKey   Key           `yaml:"cacheKey"`
	CacheValue Value         `yaml:"cacheValue"`
	MinStale   time.Duration // computed=time.Duration(float64(TTL/ErrorTTL) * Beta)
}

type Key struct {
	Query      []string `yaml:"query"` // Параметры, которые будут участвовать в ключе кэширования
	QueryBytes [][]byte
	Headers    []string `yaml:"headers"` // Хедеры, которые будут участвовать в ключе кэширования
	HeadersMap map[string]struct{}
}

type Value struct {
	Headers    []string `yaml:"headers"` // Хедеры ответа, которые будут сохранены в кэше вместе с body
	HeadersMap map[string]struct{}
}

func LoadConfig(path string) (*Cache, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	path, err = filepath.Abs(filepath.Clean(dir + path))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute config filepath: %w", err)
	}

	if _, err = os.Stat(path); err != nil {
		return nil, fmt.Errorf("stat config path: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config yaml file %s: %w", path, err)
	}

	var cfg *Cache
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal yaml from %s: %w", path, err)
	}

	for rulePath, rule := range cfg.Cache.Rules {
		rule.PathBytes = []byte(rulePath)

		// Query
		for _, query := range rule.CacheKey.Query {
			rule.CacheKey.QueryBytes = append(rule.CacheKey.QueryBytes, []byte(query))
		}

		// Request headers
		keyHeadersMap := make(map[string]struct{}, len(rule.CacheKey.Headers))
		for _, header := range rule.CacheKey.Headers {
			keyHeadersMap[header] = struct{}{}
		}
		rule.CacheKey.HeadersMap = keyHeadersMap

		// Response headers
		valueHeadersMap := make(map[string]struct{}, len(rule.CacheValue.Headers))
		for _, header := range rule.CacheValue.Headers {
			valueHeadersMap[header] = struct{}{}
		}
		rule.CacheValue.HeadersMap = valueHeadersMap

		// Other
		rule.MinStale = time.Duration(float64(rule.TTL) * rule.Beta)
	}

	cfg.Cache.Refresh.MinStale = time.Duration(float64(cfg.Cache.Refresh.TTL) * cfg.Cache.Refresh.Beta)

	cfg.Cache.LifeTime.EscapeMaxReqDurationHeaderBytes = []byte(cfg.Cache.LifeTime.EscapeMaxReqDurationHeader)

	return cfg, nil
}
