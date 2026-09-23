package rclone

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type backendSchema struct {
	prefix  string
	options map[string]bool
}

func set(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

var (
	// UnrestrictedOptions passes every rclone option through, which makes
	// anyone who can write volume attributes or rclone-secret root on the node.
	UnrestrictedOptions bool
	// AllowedBackends restricts backendSchemas further, empty allows all of them.
	AllowedBackends []string
	// AllowedEndpoints restricts endpoint options to these hosts, and entries
	// starting with "." to their subdomains. Empty allows any host.
	AllowedEndpoints []string
)

// Mount and VFS flags without file, command or network side effects
var mountFlags = set(
	"allow-non-empty", "allow-other", "async-read", "attr-timeout", "default-permissions", "dir-cache-time",
	"dir-perms", "direct-io", "file-perms", "link-perms", "gid", "uid", "umask", "max-read-ahead",
	"mount-case-insensitive", "no-checksum", "no-modtime", "no-seek", "noappledouble", "noapplexattr",
	"poll-interval", "read-only", "volname", "write-back-cache",
	"vfs-block-norm-dupes", "vfs-cache-max-age", "vfs-cache-max-size", "vfs-cache-min-free-space",
	"vfs-cache-mode", "vfs-cache-poll-interval", "vfs-case-insensitive", "vfs-disk-space-total-size",
	"vfs-fast-fingerprint", "vfs-handle-caching", "vfs-links", "vfs-metadata-extension", "vfs-read-ahead",
	"vfs-read-chunk-size", "vfs-read-chunk-size-limit", "vfs-read-chunk-streams", "vfs-read-wait",
	"vfs-refresh", "vfs-used-is-size", "vfs-write-back", "vfs-write-wait",
	"exclude", "include", "filter", "ignore-case", "max-age", "min-age", "max-size", "min-size", "max-depth",
	"buffer-size", "bwlimit", "bwlimit-file", "checkers", "transfers", "contimeout", "timeout",
	"expect-continue-timeout", "low-level-retries", "retries", "retries-sleep", "tpslimit", "tpslimit-burst",
	"use-server-modtime", "fast-list", "user-agent", "no-check-certificate", "disable-http2",
	"multi-thread-streams", "multi-thread-cutoff", "use-mmap", "log-level", "stats", "stats-log-level",
	"cache-info-age", "cache-chunk-clean-interval",
)

// Options holding a URL or host that rclone connects to
var endpointOptions = set("endpoint", "url", "host", "auth", "storage_url", "download_url", "sas_url")

// wrapperOptions hold another remote, which must not be a local path
var wrapperOptions = set("remote")

var remoteName = regexp.MustCompile(`^[\w.+@-][\w.+@ -]*$`)

// validateOptions rejects rclone options that could read or write local files, run
// commands, reach local sockets or use the node's cloud identity.
func validateOptions(remote, configData string, flags map[string]string) error {
	if UnrestrictedOptions {
		return nil
	}
	sections, err := parseConfig(configData)
	if err != nil {
		return err
	}
	for name, section := range sections {
		schema, err := backendOf(section["type"])
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "configData remote [%s]: %v", name, err)
		}
		for key, value := range section {
			if key == "type" || key == "description" {
				continue
			}
			if err := validateOption(schema, strings.ReplaceAll(key, "-", "_"), value, sections); err != nil {
				return status.Errorf(codes.InvalidArgument, "configData remote [%s]: %v", name, err)
			}
		}
	}

	if !remoteName.MatchString(remote) {
		return status.Errorf(codes.InvalidArgument, "invalid remote %q", remote)
	}
	if _, ok := sections[remote]; !ok {
		if _, err := backendOf(remote); err != nil {
			return status.Errorf(codes.InvalidArgument, "remote %q: %v", remote, err)
		}
	}

	for key, value := range flags {
		if err := validateFlag(key, value, sections); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	return nil
}

func validateFlag(key, value string, sections map[string]map[string]string) error {
	name := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(key, "--"), "_", "-"))
	if mountFlags[name] {
		return nil
	}
	for backend := range backendSchemas {
		schema, err := backendOf(backend)
		if err != nil || !strings.HasPrefix(name, schema.prefix+"-") {
			continue
		}
		option := strings.ReplaceAll(strings.TrimPrefix(name, schema.prefix+"-"), "-", "_")
		if schema.options[option] {
			return validateOption(schema, option, value, sections)
		}
	}
	return fmt.Errorf("rclone option %q is not allowed", key)
}

func validateOption(schema backendSchema, option, value string, sections map[string]map[string]string) error {
	if !schema.options[option] {
		return fmt.Errorf("option %q is not allowed", option)
	}
	if wrapperOptions[option] {
		return validateWrappedRemote(value, sections)
	}
	if endpointOptions[option] {
		return validateEndpoint(option, value)
	}
	return nil
}

// validateWrappedRemote accepts "name:path" of a configData remote or ":type:path".
// A bare path is the local backend, and ":type,key=value:" sets options unchecked.
func validateWrappedRemote(value string, sections map[string]map[string]string) error {
	name, _, ok := strings.Cut(strings.TrimPrefix(value, ":"), ":")
	if !ok || !remoteName.MatchString(name) {
		return fmt.Errorf("remote %q must be a configData remote or :type: followed by a path", value)
	}
	backend := name
	if !strings.HasPrefix(value, ":") {
		section, ok := sections[name]
		if !ok {
			return fmt.Errorf("remote %q refers to %q, which is not in configData", value, name)
		}
		backend = section["type"]
	}
	schema, err := backendOf(backend)
	if err != nil {
		return fmt.Errorf("remote %q: %v", value, err)
	}
	// Wrapping a wrapper allows cycles, which hang the mount
	for option := range wrapperOptions {
		if schema.options[option] {
			return fmt.Errorf("remote %q wraps another wrapping remote", value)
		}
	}
	return nil
}

// ponytail: checks the configured host only, rclone still follows redirects and
// resolves DNS itself. Firewall the node for a hard boundary.
func validateEndpoint(option, value string) error {
	if len(AllowedEndpoints) == 0 {
		return nil
	}
	host := value
	if u, err := url.Parse(value); err == nil && u.Host != "" {
		host = u.Hostname()
	} else if h, _, err := net.SplitHostPort(value); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	for _, allowed := range AllowedEndpoints {
		if host == allowed || (strings.HasPrefix(allowed, ".") && strings.HasSuffix(host, allowed)) {
			return nil
		}
	}
	return fmt.Errorf("%s host %q is not in --allowed-endpoints", option, host)
}

// backendOf accepts the config type or the flag prefix, like rclone does for ":type:" remotes.
func backendOf(backend string) (backendSchema, error) {
	for name, schema := range backendSchemas {
		if backend != name && backend != schema.prefix {
			continue
		}
		if len(AllowedBackends) == 0 {
			return schema, nil
		}
		for _, allowed := range AllowedBackends {
			if allowed == name || allowed == schema.prefix {
				return schema, nil
			}
		}
	}
	return backendSchema{}, fmt.Errorf("backend %q is not allowed", backend)
}

// parseConfig reads the rclone config format: [remote] sections of key = value lines.
func parseConfig(configData string) (map[string]map[string]string, error) {
	sections := map[string]map[string]string{}
	var current map[string]string
	scanner := bufio.NewScanner(strings.NewReader(configData))
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			name := strings.TrimSpace(line[1 : len(line)-1])
			if !remoteName.MatchString(name) {
				return nil, status.Errorf(codes.InvalidArgument, "configData line %d: invalid remote name %q", n, name)
			}
			if _, ok := sections[name]; ok {
				return nil, status.Errorf(codes.InvalidArgument, "configData line %d: duplicate remote [%s]", n, name)
			}
			current = map[string]string{}
			sections[name] = current
		default:
			key, value, ok := strings.Cut(line, "=")
			if !ok || current == nil {
				return nil, status.Errorf(codes.InvalidArgument, "configData line %d: expected [remote] or key = value", n)
			}
			current[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
		}
	}
	return sections, scanner.Err()
}
