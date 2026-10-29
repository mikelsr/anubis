package policy

import (
	"io"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/TecharoHQ/anubis"
	"github.com/TecharoHQ/anubis/data"
	"github.com/TecharoHQ/anubis/lib/config"
	"github.com/TecharoHQ/anubis/lib/geoip/geoiptest"
)

var scenarioPolicies = []struct {
	name string
	open func() (io.ReadCloser, error)
}{
	{
		name: "default",
		open: func() (io.ReadCloser, error) { return data.BotPolicies.Open("botPolicies.yaml") },
	},
	{name: "default-config", open: importPolicy("(data)/meta/default-config.yaml")},
	{name: "ai-block-aggressive", open: importPolicy("(data)/meta/ai-block-aggressive.yaml")},
	{name: "ai-block-moderate", open: importPolicy("(data)/meta/ai-block-moderate.yaml")},
	{name: "ai-block-permissive", open: importPolicy("(data)/meta/ai-block-permissive.yaml")},
	{name: "all-bots", open: importPolicy("(data)/bots/*.yaml")},
}

func importPolicy(path string) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("bots:\n  - import: " + path + "\n")), nil
	}
}

var scenarioRequests = []struct {
	name      string
	userAgent string
	browser   bool
}{
	{
		name:      "firefox",
		userAgent: "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0",
		browser:   true,
	},
	{
		name:      "chrome",
		userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
		browser:   true,
	},
	{
		name:      "safari_ios",
		userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Mobile/15E148 Safari/604.1",
		browser:   true,
	},
	{
		name:      "gptbot",
		userAgent: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.2; +https://openai.com/gptbot)",
	},
	{
		name:      "claudebot",
		userAgent: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)",
	},
	{
		name:      "googlebot",
		userAgent: "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
	},
	{
		name:      "curl",
		userAgent: "curl/8.14.1",
	},
	{
		name:      "empty",
		userAgent: "",
	},
}

func parseScenarioPolicy(tb testing.TB, open func() (io.ReadCloser, error)) *ParsedConfig {
	tb.Helper()

	fin, err := open()
	if err != nil {
		tb.Fatal(err)
	}
	defer fin.Close() //nolint:errcheck

	pc, err := ParseConfig(geoiptest.WithMockGeoIP(tb), fin, "bench.yaml", anubis.DefaultDifficulty, "error", false)
	if err != nil {
		tb.Fatalf("can't parse config: %v", err)
	}

	return pc
}

func newScenarioRequest(tb testing.TB, userAgent string, browser bool) *http.Request {
	tb.Helper()

	r, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	if err != nil {
		tb.Fatalf("can't make request: %v", err)
	}
	r.Header.Set("User-Agent", userAgent)
	r.Header.Set("X-Real-IP", "127.0.0.1")
	if browser {
		r.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		r.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
		r.Header.Set("Accept-Language", "en-US,en;q=0.5")
	} else {
		r.Header.Set("Accept", "*/*")
	}

	return r
}

func checkBots(tb testing.TB, pc *ParsedConfig, r *http.Request) {
	// No tb.Helper: it would add its own cost to every benchmark iteration
	for i := range pc.Bots {
		bot := &pc.Bots[i]
		ok, err := bot.Rules.Check(r)
		if err != nil {
			tb.Fatal(err)
		}

		if ok && bot.Action != config.RuleWeigh {
			return
		}
	}
}

func BenchmarkPolicyScenarios(b *testing.B) {
	for _, p := range scenarioPolicies {
		pc := parseScenarioPolicy(b, p.open)
		for _, req := range scenarioRequests {
			b.Run(p.name+"/"+req.name, func(b *testing.B) {
				r := newScenarioRequest(b, req.userAgent, req.browser)
				for b.Loop() {
					checkBots(b, pc, r)
				}
			})
		}
	}
}

func BenchmarkDefaultPolicyRules(b *testing.B) {
	pc := parseScenarioPolicy(b, scenarioPolicies[0].open)
	r := newScenarioRequest(b, scenarioRequests[0].userAgent, true)
	for i := range pc.Bots {
		bot := &pc.Bots[i]
		b.Run(bot.Name, func(b *testing.B) {
			for b.Loop() {
				if _, err := bot.Rules.Check(r); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkLongUserAgents(b *testing.B) {
	const browser = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0 "

	pc := parseScenarioPolicy(b, importPolicy("(data)/bots/*.yaml"))

	// Each name is the branch a split regex checks last, so its slowest match
	for _, rule := range []struct {
		name string
		last string
	}{
		{name: "ai-catchall", last: "iaskspider/2.0"},
		{name: "ai-robots-txt", last: "quillbot.com"},
	} {
		i := slices.IndexFunc(pc.Bots, func(bot Bot) bool { return bot.Name == rule.name })
		if i < 0 {
			b.Fatalf("no rule named %s", rule.name)
		}
		bot := &pc.Bots[i]

		for _, size := range []struct {
			name string
			len  int
		}{
			{name: "short", len: len(browser)},
			{name: "8KiB", len: 8 << 10},
		} {
			text := strings.Repeat(browser, size.len/len(browser)+1)[:size.len]
			for _, input := range []struct {
				name      string
				userAgent string
				match     bool
			}{
				{name: "no_match", userAgent: text},
				{name: "match_at_start", userAgent: rule.last + " " + text, match: true},
			} {
				b.Run(rule.name+"/"+input.name+"/"+size.name, func(b *testing.B) {
					r := newScenarioRequest(b, input.userAgent, false)
					if ok, err := bot.Rules.Check(r); err != nil || ok != input.match {
						b.Fatalf("match: %v (%v), wanted: %v", ok, err, input.match)
					}

					for b.Loop() {
						if _, err := bot.Rules.Check(r); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

func BenchmarkParsePolicy(b *testing.B) {
	for _, p := range scenarioPolicies {
		b.Run(p.name, func(b *testing.B) {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			pc := parseScenarioPolicy(b, p.open)
			runtime.GC()
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(pc)

			b.ReportAllocs()
			for b.Loop() {
				parseScenarioPolicy(b, p.open)
			}

			// b.Loop resets reported metrics when it starts, so report after it
			b.ReportMetric(float64(after.HeapAlloc)-float64(before.HeapAlloc), "retained-B")
		})
	}
}
