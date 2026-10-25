package workspace

import (
	"fmt"
	"sort"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// TopoLevels orders service names into dependency levels: level 0 holds
// services with no dependencies, level N holds services whose dependencies
// all appear in earlier levels. Names inside one level are sorted, so both
// the levels and the parallel work they unlock are deterministic. A
// dependency cycle or a reference to an unknown service fails the call.
func TopoLevels(names []string, deps map[string][]string) ([][]string, error) {
	known := make(map[string]bool, len(names))
	for _, name := range names {
		known[name] = true
	}
	indegree := make(map[string]int, len(names))
	dependents := make(map[string][]string, len(names))
	for _, name := range names {
		indegree[name] = 0
	}
	for name, list := range deps {
		if !known[name] {
			continue
		}
		for _, dep := range list {
			if !known[dep] {
				return nil, phelixerr.Newf(
					phelixerr.CodeValidation,
					"service %q depends on unknown service %q",
					name, dep,
				)
			}
			indegree[name]++
			dependents[dep] = append(dependents[dep], name)
		}
	}

	var ready []string
	for _, name := range names {
		if indegree[name] == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)

	var levels [][]string
	placed := 0
	for len(ready) > 0 {
		level := ready
		levels = append(levels, level)
		placed += len(level)
		var next []string
		for _, name := range level {
			for _, dependent := range dependents[name] {
				indegree[dependent]--
				if indegree[dependent] == 0 {
					next = append(next, dependent)
				}
			}
		}
		sort.Strings(next)
		ready = next
	}

	if placed < len(names) {
		return nil, phelixerr.Newf(
			phelixerr.CodeValidation,
			"dependency cycle among services: %s\nHint: remove one depends_on edge to break the cycle",
			strings.Join(cyclePath(names, deps), " -> "),
		)
	}
	return levels, nil
}

// TopoOrder flattens the dependency levels into one execution order where
// every service appears after all of its dependencies.
func TopoOrder(names []string, deps map[string][]string) ([]string, error) {
	levels, err := TopoLevels(names, deps)
	if err != nil {
		return nil, err
	}
	var order []string
	for _, level := range levels {
		order = append(order, level...)
	}
	return order, nil
}

// cyclePath traces one concrete dependency cycle for the error message. It
// is only called when a cycle is known to exist.
func cyclePath(names []string, deps map[string][]string) []string {
	inSet := make(map[string]bool, len(names))
	for _, name := range names {
		inSet[name] = true
	}
	var path []string
	for _, name := range names {
		visited := map[string]bool{}
		var walk func(node string) bool
		walk = func(node string) bool {
			if visited[node] {
				if indexOf(path, node) >= 0 {
					path = append(path, node)
					return true
				}
				return false
			}
			visited[node] = true
			path = append(path, node)
			for _, dep := range deps[node] {
				if !inSet[dep] {
					continue
				}
				if walk(dep) {
					return true
				}
			}
			path = path[:len(path)-1]
			return false
		}
		if walk(name) {
			start := indexOf(path, name)
			if start < 0 {
				start = 0
			}
			return path[start : len(path)-1]
		}
	}
	return names
}

func indexOf(list []string, want string) int {
	for i, item := range list {
		if item == want {
			return i
		}
	}
	return -1
}

// duplicatePort reports the first service pair sharing an explicitly
// declared port, or an empty string when ports are unique.
func duplicatePort(services []*Service) string {
	owner := map[int]string{}
	for _, svc := range services {
		if svc.Port == 0 {
			continue
		}
		if prev, clash := owner[svc.Port]; clash {
			return fmt.Sprintf("%s and %s both declare port %d", prev, svc.Name, svc.Port)
		}
		owner[svc.Port] = svc.Name
	}
	return ""
}
