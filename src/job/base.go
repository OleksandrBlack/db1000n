// MIT License

// Copyright (c) [2022] [Bohdan Ivashko (https://github.com/Arriven)]

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

// Package job [contains all the attack types db1000n can simulate]
package job

import (
	"context"
	"flag"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Arriven/db1000n/src/job/config"
	"github.com/Arriven/db1000n/src/utils"
	"github.com/Arriven/db1000n/src/utils/metrics"
	"github.com/Arriven/db1000n/src/utils/templates"
)

// GlobalConfig passes commandline arguments to every job.
type GlobalConfig struct {
	ClientID string
	UserID   string
	Source   string

	proxyURLs           string
	proxylist           string
	defaultProxyProto   string
	localAddr           string
	iface               string
	SkipEncrypted       bool
	EnablePrimitiveJobs bool
	ScaleFactor         float64
	RandomInterval      time.Duration
	MinInterval         time.Duration
	Backoff             utils.BackoffConfig
}

// NewGlobalConfigWithFlags returns a GlobalConfig initialized with command line flags.
func NewGlobalConfigWithFlags() *GlobalConfig {
	res := GlobalConfig{
		ClientID: uuid.NewString(),
	}

	flag.StringVar(&res.UserID, "user-id", utils.GetEnvStringDefault("USER_ID", ""),
		"user id for optional metrics")
	flag.StringVar(&res.Source, "source", utils.GetEnvStringDefault("SOURCE", "single"),
		"run source info")
	flag.StringVar(&res.proxyURLs, "proxy", utils.GetEnvStringDefault("SYSTEM_PROXY", ""),
		"system proxy to set by default (can be a comma-separated list or a template)")
	flag.StringVar(&res.proxylist, "proxylist", "", "file or url to read a list of proxies from")
	flag.StringVar(&res.defaultProxyProto, "default-proxy-proto", "socks5", "protocol to fallback to if proxy contains only address")
	flag.StringVar(&res.localAddr, "local-address", utils.GetEnvStringDefault("LOCAL_ADDRESS", ""),
		"specify ip address of local interface to use")
	flag.StringVar(&res.iface, "interface", utils.GetEnvStringDefault("NETWORK_INTERFACE", ""),
		"specify which interface to bind to for attacks (ignored on windows)")
	flag.BoolVar(&res.SkipEncrypted, "skip-encrypted", utils.GetEnvBoolDefault("SKIP_ENCRYPTED", false),
		"set to true if you want to only run plaintext jobs from the config for security considerations")
	flag.BoolVar(&res.EnablePrimitiveJobs, "enable-primitive", utils.GetEnvBoolDefault("ENABLE_PRIMITIVE", true),
		"set to true if you want to run primitive jobs that are less resource-efficient")
	flag.Float64Var(&res.ScaleFactor, "scale", utils.GetEnvFloatDefault("SCALE_FACTOR", 1.0),
		"used to scale the amount of jobs being launched, effect is similar to launching multiple instances at once")
	flag.DurationVar(&res.RandomInterval, "random-interval", utils.GetEnvDurationDefault("RANDOM_INTERVAL", 0),
		"random interval to add between job iterations")
	flag.DurationVar(&res.MinInterval, "min-interval", utils.GetEnvDurationDefault("MIN_INTERVAL", 0),
		"minimum interval between job iterations")

	flag.IntVar(&res.Backoff.Limit, "backoff-limit", utils.GetEnvIntDefault("BACKOFF_LIMIT", utils.DefaultBackoffConfig().Limit),
		"how much exponential backoff can be scaled")
	flag.IntVar(&res.Backoff.Multiplier, "backoff-multiplier", utils.GetEnvIntDefault("BACKOFF_MULTIPLIER", utils.DefaultBackoffConfig().Multiplier),
		"how much exponential backoff is scaled with each new error")
	flag.DurationVar(&res.Backoff.Timeout, "backoff-timeout", utils.GetEnvDurationDefault("BACKOFF_TIMEOUT", utils.DefaultBackoffConfig().Timeout),
		"initial exponential backoff timeout")

	return &res
}

func (g GlobalConfig) GetProxyParams(logger *zap.Logger, data any) utils.ProxyParams {
	return utils.ProxyParams{
		URLs:         templates.ParseAndExecute(logger, g.proxyURLs, data),
		DefaultProto: g.defaultProxyProto,
		LocalAddr:    templates.ParseAndExecute(logger, g.localAddr, data),
		Interface:    templates.ParseAndExecute(logger, g.iface, data),
	}
}

func (g *GlobalConfig) initProxylist(ctx context.Context) error {
	if g.proxyURLs != "" || g.proxylist == "" {
		return nil
	}

	proxylist, err := readProxylist(ctx, g.proxylist)
	if err != nil {
		return err
	}

	g.proxyURLs = string(proxylist)

	return nil
}

func readProxylist(ctx context.Context, path string) ([]byte, error) {
	proxylistURL, err := url.ParseRequestURI(path)
	// absolute paths can be interpreted as a URL with no schema, need to check for that explicitly
	if err != nil || filepath.IsAbs(path) {
		res, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}

		return res, nil
	}

	const requestTimeout = 20 * time.Second

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxylistURL.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

// Job comment for linter
type Job = func(ctx context.Context, args config.Args, globalConfig *GlobalConfig, a *metrics.Accumulator, logger *zap.Logger) (data any, err error)

// Get job by type name
//
//nolint:cyclop // The string map alternative is orders of magnitude slower
func Get(t string) Job {
	switch t {
	case "http", "http-flood":
		return fastHTTPJob
	case "http-request":
		return singleRequestJob
	case "tcp":
		return tcpJob
	case "udp":
		return udpJob
	case "packetgen":
		return packetgenJob
	case "sequence":
		return sequenceJob
	case "parallel":
		return parallelJob
	case "log":
		return logJob
	case "set-value":
		return setVarJob
	case "check":
		return checkJob
	case "sleep":
		return sleepJob
	case "discard-error":
		return discardErrorJob
	case "timeout":
		return timeoutJob
	case "loop":
		return loopJob
	case "lock":
		return lockJob
	case "js":
		return jsJob
	case "encrypted":
		return encryptedJob
	default:
		return nil
	}
}

type Config interface {
	FromGlobal(GlobalConfig)
}

func ParseConfig(c Config, args config.Args, global GlobalConfig) error {
	if err := utils.Decode(args, c); err != nil {
		return err
	}

	c.FromGlobal(global)

	return nil
}

// BasicJobConfig comment for linter
type BasicJobConfig struct {
	IntervalMs     int
	Interval       *time.Duration
	RandomInterval time.Duration
	utils.Counter
	Backoff *utils.BackoffConfig
}

func (c *BasicJobConfig) FromGlobal(global GlobalConfig) {
	if c.GetInterval(true) < global.MinInterval {
		c.Interval = &global.MinInterval
	}

	if c.RandomInterval < global.RandomInterval {
		c.RandomInterval = global.RandomInterval
	}

	if c.Backoff == nil {
		c.Backoff = &global.Backoff
	}
}

func (c BasicJobConfig) GetInterval(stable bool) time.Duration {
	stableInterval := utils.NonNilOrDefault(c.Interval, time.Duration(c.IntervalMs)*time.Millisecond)
	if stable {
		return stableInterval
	}

	return stableInterval + time.Duration(rand.Int63n(utils.Max(c.RandomInterval.Nanoseconds(), 1)))
}

// Next comment for linter
func (c *BasicJobConfig) Next(ctx context.Context) bool {
	return utils.Sleep(ctx, c.GetInterval(false)) && c.Counter.Next()
}
