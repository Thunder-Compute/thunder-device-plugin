package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

func thunderdEnvFromEnviron(environ []string) (map[string]string, error) {
	values := make(map[string]string)
	for _, entry := range environ {
		name, value, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, EnvThunderdPrefix) {
			continue
		}
		key := strings.TrimPrefix(name, EnvThunderdPrefix)
		if !validEnvKey(key) {
			return nil, fmt.Errorf("invalid thunderd environment variable name %q", name)
		}
		if !validEnvValue(value) {
			return nil, fmt.Errorf("invalid environment file value for %s", name)
		}
		values[key] = value
	}
	return values, nil
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, c := range key {
		if c != '_' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func validEnvValue(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, c := range value {
		if c == 0 || c == '\uFEFF' || c >= 0xFDD0 && c <= 0xFDEF || c&0xFFFF >= 0xFFFE {
			return false
		}
	}
	return true
}

// configureThunderdEnv is called only at process startup. The host mount is
// read-only; the existing runner performs the atomic replacement on the host.
func configureThunderdEnv(ctx context.Context, cfg Config, runner commandRunner) error {
	if len(cfg.ThunderdEnv) == 0 {
		return nil
	}
	data, err := os.ReadFile(resolveNodePath(cfg.HostRoot, thunderdEnvPath))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read thunderd environment: %w", err)
	}
	merged, err := mergeThunderdEnv(string(data), cfg.ThunderdEnv)
	if err != nil {
		return err
	}
	if merged == string(data) {
		return nil
	}
	return runner.RunShellInput(ctx, "thunderd environment configuration", thunderdEnvWriteCommand(thunderdEnvPath), merged)
}

func thunderdEnvWriteCommand(path string) string {
	return "set -eu\n" +
		"env_file=" + shellQuote(path) + "\n" +
		"mkdir -p -- \"${env_file%/*}\"\n" +
		"env_tmp=$(mktemp \"${env_file}.XXXXXX\")\n" +
		"trap 'rm -f -- \"$env_tmp\"' EXIT\n" +
		"if [ -e \"$env_file\" ]; then cp -p -- \"$env_file\" \"$env_tmp\"; fi\n" +
		"cat > \"$env_tmp\"\n" +
		"mv -f -- \"$env_tmp\" \"$env_file\"\n"
}

// Preserve original records, including comments and multiline values. Only the
// last assignment to each supplied key needs updating: systemd uses the last
// value. Comparing parsed values also avoids rewriting equivalent quoting.
func mergeThunderdEnv(data string, desired map[string]string) (string, error) {
	type record struct{ raw, value string }
	var records []record
	last := make(map[string]int)
	for rest := data; rest != ""; {
		line, _, _ := strings.Cut(rest, "\n")
		length := len(line)
		if length < len(rest) {
			length++
		}
		trimmed := strings.TrimSpace(line)
		key, _, assignment := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		var value string
		if assignment && validEnvKey(key) && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, ";") {
			var err error
			value, length, err = parseEnvFileValue(rest, strings.Index(line, "=")+1)
			if err != nil {
				return "", fmt.Errorf("parse thunderd environment key %s: %w", key, err)
			}
			last[key] = len(records)
		} else {
			key = ""
		}
		records = append(records, record{rest[:length], value})
		rest = rest[length:]
	}
	keys := make([]string, 0, len(desired))
	for key, value := range desired {
		if !validEnvKey(key) || !validEnvValue(value) {
			return "", fmt.Errorf("invalid thunderd environment setting %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var additions strings.Builder
	for _, key := range keys {
		value := desired[key]
		if i, found := last[key]; found {
			if records[i].value != value {
				records[i].raw = key + "=" + quoteEnvFileValue(value) + "\n"
			}
		} else {
			additions.WriteString(key + "=" + quoteEnvFileValue(value) + "\n")
		}
	}
	var result strings.Builder
	for _, record := range records {
		result.WriteString(record.raw)
	}
	if additions.Len() > 0 {
		if result.Len() > 0 && !strings.HasSuffix(result.String(), "\n") {
			result.WriteByte('\n')
		}
		result.WriteString(additions.String())
	}
	return result.String(), nil
}

// EnvironmentFile uses shell-style escaping, without variable expansion.
// Double quotes preserve literal newlines and leading/trailing whitespace.
func quoteEnvFileValue(value string) string {
	if value == "" {
		return ""
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(value) + `"`
}

func parseEnvFileValue(data string, start int) (string, int, error) {
	i := start
	for i < len(data) && strings.ContainsRune(" \t\r", rune(data[i])) {
		i++
	}
	var quote byte
	if i < len(data) && (data[i] == '\'' || data[i] == '"') {
		quote = data[i]
		i++
	}
	quoted := quote != 0
	var value strings.Builder
	valueEnd := 0
	for i < len(data) {
		c := data[i]
		i++
		if c == '\n' && quote == 0 {
			break
		}
		if quote != 0 && c == quote {
			quote = 0
			// Only whitespace may follow the closing quote.
			for i < len(data) && data[i] != '\n' {
				if !strings.ContainsRune(" \t\r", rune(data[i])) {
					return "", 0, fmt.Errorf("unexpected text after quoted value")
				}
				i++
			}
			if i < len(data) {
				i++
			}
			break
		}
		if c == '\\' && quote != '\'' && i < len(data) {
			next := data[i]
			i++
			if next == '\n' {
				continue
			}
			if quote == '"' && !strings.ContainsRune("\"\\`$", rune(next)) {
				value.WriteByte('\\')
			}
			value.WriteByte(next)
			valueEnd = value.Len()
			continue
		}
		value.WriteByte(c)
		if !strings.ContainsRune(" \t\r", rune(c)) {
			valueEnd = value.Len()
		}
	}
	if quote != 0 {
		return "", 0, fmt.Errorf("unterminated quoted value")
	}
	if !quoted {
		return value.String()[:valueEnd], i, nil
	}
	return value.String(), i, nil
}
