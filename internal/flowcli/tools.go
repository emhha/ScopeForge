// Package flowcli adapts installed recon commands to structured native tools.
// Graph scheduling stays in flowproto / the native GraphAgent.
package flowcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

type Execute func(context.Context, []byte) (any, error)
type DomainInput struct {
	Domain string `json:"domain"`
}
type Domains struct {
	Subdomains []string `json:"subdomains"`
}
type MergeInput struct {
	Domain string     `json:"domain"`
	Lists  [][]string `json:"lists"`
}
type HostsInput struct {
	Hosts []string `json:"hosts"`
}
type URLs struct {
	URLs []string `json:"urls"`
}
type Fingerprints struct {
	Fingerprints any `json:"fingerprints"`
}

var label = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func domain(value string) (string, error) {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	parts := strings.Split(value, ".")
	if len(value) > 253 || len(parts) < 2 {
		return "", fmt.Errorf("expected a domain name, got %q", value)
	}
	for _, part := range parts {
		if !label.MatchString(part) {
			return "", fmt.Errorf("invalid domain %q", value)
		}
	}
	return value, nil
}
func unique(values []string) []string {
	slices.Sort(values)
	return slices.Compact(values)
}
func names(text, root string) []string {
	result := []string{}
	for _, line := range strings.Split(text, "\n") {
		name, err := domain(line)
		if err == nil && (name == root || strings.HasSuffix(name, "."+root)) {
			result = append(result, name)
		}
	}
	return unique(result)
}
func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// Each invocation owns its files. Full machine output is read from an artifact,
// never from the host executor's possibly shortened console preview.
func run(ctx context.Context, execute Execute, program string, args func(string) []string, input string, ehole bool) ([]byte, error) {
	dir, err := os.MkdirTemp("", "scopeforge-flow-cli-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte(input), 0600); err != nil {
		return nil, err
	}
	argv := append([]string{program}, args(dir)...)
	for i := range argv {
		argv[i] = quote(argv[i])
	}
	raw, _ := json.Marshal(map[string]any{"command": strings.Join(argv, " "), "yield_time_ms": 1000})
	result, err := execute(ctx, raw)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	failed := toolkit.Failed(result)
	// Upstream EHole finger exits 1 even after a successful scan. Accept that
	// only when a valid JSON result file was actually produced.
	knownEholeExit := false
	if value, ok := result.(map[string]any); ok && ehole {
		code := value["exit_code"]
		knownEholeExit = code == 1 || code == float64(1) || code == int64(1)
	}
	if failed && !knownEholeExit {
		return nil, fmt.Errorf("%s failed: %s", program, toolkit.TextOf(result))
	}
	output := filepath.Join(dir, "output")
	if ehole {
		output += ".json"
	}
	file, err := os.Open(output)
	if err != nil {
		return nil, fmt.Errorf("%s did not produce output: %w", program, err)
	}
	defer file.Close()
	const limit = 32 << 20
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%s output exceeds 32 MiB", program)
	}
	if knownEholeExit && !json.Valid(data) {
		return nil, fmt.Errorf("ehole exited 1 without valid JSON: %s", toolkit.TextOf(result))
	}
	return data, nil
}

func Tools(execute Execute) []tool.CallableTool {
	out := []tool.CallableTool{}
	for _, name := range []string{"subfinder", "amass"} {
		out = append(out, function.NewFunctionTool(func(ctx context.Context, in DomainInput) (Domains, error) {
			root, err := domain(in.Domain)
			if err != nil {
				return Domains{}, err
			}
			data, err := run(ctx, execute, name, func(dir string) []string {
				if name == "subfinder" {
					return []string{"-d", root, "-silent", "-o", filepath.Join(dir, "output")}
				}
				return []string{"enum", "-passive", "-d", root, "-dir", filepath.Join(dir, "amass"), "-o", filepath.Join(dir, "output")}
			}, "", false)
			if err != nil {
				return Domains{}, err
			}
			return Domains{names(string(data), root)}, nil
		}, function.WithName(name), function.WithDescription("运行已安装的 "+name+" 被动枚举域名，返回结构化 subdomains 数组；使用现有 CLI 配置")))
	}
	out = append(out, function.NewFunctionTool(func(_ context.Context, in MergeInput) (Domains, error) {
		root, err := domain(in.Domain)
		if err != nil {
			return Domains{}, err
		}
		all := []string{}
		for _, list := range in.Lists {
			all = append(all, list...)
		}
		return Domains{names(strings.Join(all, "\n"), root)}, nil
	}, function.WithName("merge_subdomains"), function.WithDescription("合并并去重枚举结果，仅保留输入域名及其子域名")))
	out = append(out, function.NewFunctionTool(func(ctx context.Context, in HostsInput) (URLs, error) {
		hosts := []string{}
		for _, host := range in.Hosts {
			normalized, err := domain(host)
			if err != nil {
				return URLs{}, err
			}
			hosts = append(hosts, normalized)
		}
		if len(hosts) == 0 {
			return URLs{[]string{}}, nil
		}
		data, err := run(ctx, execute, "httpx", func(dir string) []string {
			return []string{"-l", filepath.Join(dir, "input.txt"), "-silent", "-no-color", "-o", filepath.Join(dir, "output")}
		}, strings.Join(unique(hosts), "\n")+"\n", false)
		if err != nil {
			return URLs{}, err
		}
		urls, err := validURLs(strings.Split(string(data), "\n"))
		if err != nil {
			return URLs{}, fmt.Errorf("httpx output: %w", err)
		}
		return URLs{urls}, nil
	}, function.WithName("httpx"), function.WithDescription("调用 ProjectDiscovery httpx 探测子域名，输出存活 HTTP/HTTPS URLs；不是 Python httpx")))
	out = append(out, function.NewFunctionTool(func(ctx context.Context, in URLs) (Fingerprints, error) {
		urls, err := validURLs(in.URLs)
		if err != nil {
			return Fingerprints{}, err
		}
		if len(urls) == 0 {
			return Fingerprints{[]any{}}, nil
		}
		data, err := run(ctx, execute, "ehole", func(dir string) []string {
			return []string{"finger", "-l", filepath.Join(dir, "input.txt"), "-o", filepath.Join(dir, "output") + ".json"}
		}, strings.Join(urls, "\n")+"\n", true)
		if err != nil {
			return Fingerprints{}, err
		}
		var result any
		if err := json.Unmarshal(data, &result); err != nil {
			return Fingerprints{}, fmt.Errorf("ehole JSON: %w", err)
		}
		return Fingerprints{result}, nil
	}, function.WithName("ehole"), function.WithDescription("调用 EHole 3.x finger -l -o 识别存活 URLs，保留原始 JSON 指纹结果；使用已安装工具的默认规则")))
	return out
}

func validURLs(values []string) ([]string, error) {
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return nil, fmt.Errorf("invalid HTTP URL %q", value)
		}
		out = append(out, value)
	}
	return unique(out), nil
}
