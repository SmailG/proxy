package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/routatic/proxy/internal/config"
)

// decodeJSONMap decodes JSON without going through float64, so numbers the
// config file wrote are re-encoded exactly as they were.
func decodeJSONMap(data []byte, dst *map[string]any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(dst)
}

// mergeConfigPatch applies patch onto the config document stored at path and
// returns the JSON to write.
//
// The merge runs on the decoded file, not on the typed Config, on purpose: the
// loader interpolates ${VAR} references anywhere in the document, so
// re-serializing the loaded config would write every resolved secret back as a
// literal. Working on the raw map preserves references, keys this build does not
// know about, and the exact number encoding for everything the patch does not
// touch.
//
// Values equal to redactedSentinel are dropped rather than merged: GET hides
// secrets behind that placeholder, so a client that posts back what it was given
// must leave the real key alone instead of replacing it. Where a placeholder
// cannot be dropped safely, stripRedacted returns errRedactedEcho and nothing is
// written.
func mergeConfigPatch(path string, patch map[string]any) ([]byte, error) {
	if err := stripRedacted(patch); err != nil {
		return nil, err
	}
	rawFile, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	var raw map[string]any
	if err := decodeJSONMap(rawFile, &raw); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	mergeJSONMap(raw, patch)
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding merged config: %w", err)
	}
	return out, nil
}

// errRedactedEcho reports a patch that carries the redaction placeholder in a
// place the merge cannot reconcile, so it is refused rather than guessed at.
var errRedactedEcho = errors.New("hidden value cannot be saved as-is; type a new value to replace it")

// stripRedacted deletes redaction placeholders from patch at any depth.
//
// A placeholder may only be dropped where the merge leaves the stored value
// alone, which is true of a map key the patch omits. Lists are replaced
// wholesale by mergeJSONMap, so dropping a placeholder inside one would lose the
// stored secret, and a placeholder sitting beside real values in a list cannot be
// told apart from an intentional key. Those shapes return errRedactedEcho.
func stripRedacted(m map[string]any) error {
	for k, v := range m {
		switch t := v.(type) {
		case string:
			if t == redactedSentinel {
				delete(m, k)
			}
		case map[string]any:
			if err := stripRedacted(t); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		case []any:
			if allRedacted(t) {
				delete(m, k)
				continue
			}
			if containsRedacted(t) {
				return fmt.Errorf("%s: %w", k, errRedactedEcho)
			}
		}
	}
	return nil
}

func allRedacted(list []any) bool {
	if len(list) == 0 {
		return false
	}
	for _, v := range list {
		if s, ok := v.(string); !ok || s != redactedSentinel {
			return false
		}
	}
	return true
}

// containsRedacted reports whether the placeholder appears anywhere in v.
func containsRedacted(v any) bool {
	switch t := v.(type) {
	case string:
		return t == redactedSentinel
	case map[string]any:
		for _, child := range t {
			if containsRedacted(child) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if containsRedacted(child) {
				return true
			}
		}
	}
	return false
}

// mergeJSONMap merges src into dst: objects merge key by key, anything else
// replaces the value that was there.
func mergeJSONMap(dst, src map[string]any) {
	for k, sv := range src {
		sm, srcIsMap := sv.(map[string]any)
		dm, dstIsMap := dst[k].(map[string]any)
		if srcIsMap && dstIsMap {
			mergeJSONMap(dm, sm)
			continue
		}
		dst[k] = sv
	}
}

// writeTempConfig writes data to a 0600 file beside path so the caller can
// validate the merged document before it replaces the live config. The rename
// that follows is atomic, so a failed validation or a crash mid-write cannot
// leave a truncated config.json behind.
func writeTempConfig(path string, data []byte) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("creating temp config: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("writing temp config: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("closing temp config: %w", err)
	}
	return tmp, nil
}

// handleConfigEnv reports which env vars the stored config references and
// whether each is currently non-empty. Values are never returned — only the
// variable names and their presence.
func (s *Server) handleConfigEnv(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.atomicCfg == nil {
		http.Error(w, "config not available", http.StatusServiceUnavailable)
		return
	}

	data, err := os.ReadFile(s.atomicCfg.Path())
	if err != nil {
		s.logInternal("reading config for env refs", err)
		http.Error(w, "could not read the config file", http.StatusInternalServerError)
		return
	}
	var raw map[string]any
	if err := decodeJSONMap(data, &raw); err != nil {
		s.logInternal("parsing config for env refs", err)
		http.Error(w, "config file is not valid JSON", http.StatusInternalServerError)
		return
	}

	refs := map[string][]string{}
	collectEnvRefs("", raw, refs)
	// jev_routing.api_key_env holds a bare variable name rather than a ${VAR},
	// because it names the variable the classifier reads at call time. Append
	// rather than assign: the file may also contain a raw ${VAR} there.
	if name := s.atomicCfg.Get().JevRouting.APIKeyEnv; name != "" {
		refs["jev_routing.api_key_env"] = appendUnique(refs["jev_routing.api_key_env"], name)
	}

	env := map[string]bool{}
	for _, names := range refs {
		for _, name := range names {
			if _, seen := env[name]; !seen {
				// config.EnvValue is what the loader uses, so the status here
				// agrees with what a reload would actually resolve.
				env[name] = config.EnvValue(name) != ""
			}
		}
	}

	writeJSON(w, map[string]any{"refs": refs, "env": env})
}

func collectEnvRefs(path string, v any, out map[string][]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			childPath := k
			if path != "" {
				childPath = path + "." + k
			}
			collectEnvRefs(childPath, child, out)
		}
	case []any:
		for _, child := range t {
			collectEnvRefs(path, child, out)
		}
	case string:
		for _, m := range config.EnvVarPattern.FindAllStringSubmatch(t, -1) {
			out[path] = appendUnique(out[path], m[1])
		}
	}
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}
