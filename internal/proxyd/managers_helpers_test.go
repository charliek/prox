package proxyd

import "github.com/charliek/prox/internal/proxy"

// Test helpers for the per-project ring set (D13). The daemon now keeps one
// *proxy.RequestManager per project instead of a single global one, so tests
// that used to reach through s.requestManager go through these instead.

// recordInto records r into its project's ring, creating the ring on demand
// (mirroring what a successful register does in production). Tests that seed
// records for a project registered directly through s.registry.Register — which
// does not create a ring — use this to make the ring exist.
func recordInto(s *Server, r proxy.RequestRecord) {
	s.managers.ensure(r.ProjectDir).Record(r)
}

// projectRing returns the project's ring, or nil when it has none (never
// created, or destroyed by a genuine removal).
func projectRing(s *Server, projectDir string) *proxy.RequestManager {
	return s.managers.get(projectDir)
}

// projectCount returns the number of records in the project's ring, or 0 when
// the project has no ring.
func projectCount(s *Server, projectDir string) int {
	if m := s.managers.get(projectDir); m != nil {
		return m.Count()
	}
	return 0
}

// projectRecent returns the project's records (newest first), or nil when the
// project has no ring.
func projectRecent(s *Server, projectDir string) []proxy.RequestRecord {
	if m := s.managers.get(projectDir); m != nil {
		return m.Recent(proxy.RequestFilter{ProjectDir: projectDir})
	}
	return nil
}

// projectHas reports whether the project's ring holds a record with the given
// ID. False when the project has no ring.
func projectHas(s *Server, projectDir, id string) bool {
	if m := s.managers.get(projectDir); m != nil {
		_, ok := m.GetByID(id)
		return ok
	}
	return false
}
