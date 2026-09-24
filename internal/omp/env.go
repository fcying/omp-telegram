package omp

import (
	"errors"
	"os"
	"strings"
)

const botTokenName = "OMP_TELEGRAM_BOT_TOKEN"

// Environment controls which parent variables native omp children receive.
// Its zero value inherits every variable except the bridge bot token.
type Environment struct {
	allowlist bool
	allowed   map[string]struct{}
	denied    map[string]struct{}
}

// NewEnvironment constructs a denylist or allowlist policy for native children.
func NewEnvironment(mode string, allow, deny []string) (Environment, error) {
	switch mode {
	case "denylist":
		if allow != nil {
			return Environment{}, errors.New("omp: denylist environment cannot specify an allow list")
		}
		if len(deny) == 0 {
			return Environment{}, nil
		}
		denied := make(map[string]struct{}, len(deny))
		for _, name := range deny {
			if !validEnvironmentName(name) {
				return Environment{}, errors.New("omp: invalid denied environment variable")
			}
			denied[name] = struct{}{}
		}
		return Environment{denied: denied}, nil
	case "allowlist":
		if deny != nil {
			return Environment{}, errors.New("omp: allowlist environment cannot specify a deny list")
		}
		if len(allow) == 0 {
			return Environment{allowlist: true}, nil
		}
		allowed := make(map[string]struct{}, len(allow))
		for _, name := range allow {
			if !validEnvironmentName(name) || name == botTokenName {
				return Environment{}, errors.New("omp: invalid allowed environment variable")
			}
			allowed[name] = struct{}{}
		}
		return Environment{allowlist: true, allowed: allowed}, nil
	default:
		return Environment{}, errors.New("omp: invalid environment mode")
	}
}

func validEnvironmentName(name string) bool {
	if name == "" || !(name[0] >= 'A' && name[0] <= 'Z' || name[0] >= 'a' && name[0] <= 'z' || name[0] == '_') {
		return false
	}
	for i := 1; i < len(name); i++ {
		ch := name[i]
		if !(ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_') {
			return false
		}
	}
	return true
}

func (environment Environment) permits(name string) bool {
	if name == botTokenName {
		return false
	}
	if _, denied := environment.denied[name]; denied {
		return false
	}
	if environment.allowlist {
		_, allowed := environment.allowed[name]
		return allowed
	}
	return true
}

// Lookup returns a parent variable only if the policy permits it.
func (environment Environment) Lookup(name string) (string, bool) {
	if !environment.permits(name) {
		return "", false
	}
	return os.LookupEnv(name)
}

// ChildEnv returns an explicit child environment filtered by the policy.
func ChildEnv(environment Environment) []string {
	env := os.Environ()
	filtered := env[:0]
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !environment.permits(name) {
			continue
		}
		filtered = append(filtered, entry)
	}
	if filtered == nil {
		return []string{}
	}
	return filtered
}
