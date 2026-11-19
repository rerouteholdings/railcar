package chat

import (
	"slices"
	"strings"
)

// CommunityView is what the interface shows of a community.
type CommunityView struct {
	Server string
	Name   string
	Desc   string
	Owner  string
	// Perms are this identity's permissions (§16.3).
	Perms []string
	// Roles are the roles by ID, with the names this device can open.
	Roles map[uint64]string
	// Members are the members group's identities, each with the names of
	// their roles.
	Members map[string][]string
	Stopped string
	// Public reports whether anyone with a code can join and read, which
	// the join preview has to say (§16.4).
	Public bool
}

// Community returns a community's view, by its server.
func (e *Engine) Community(server string) (CommunityView, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cm := e.st.Communities[server]
	if cm == nil {
		return CommunityView{}, false
	}
	v := CommunityView{Server: server, Stopped: cm.Stopped, Roles: map[uint64]string{}, Members: map[string][]string{}}
	c, err := e.communityChain(cm)
	if err != nil {
		return v, true
	}
	st := c.State()
	v.Name, v.Desc, v.Owner, v.Public = st.Name, st.Desc, st.Owner, st.Public
	v.Perms = st.Perms(e.c.ID())
	for id := range st.Roles {
		v.Roles[id] = e.roleName(cm, st, id)
	}
	if g := cm.Groups[cm.MembersGroup]; g != nil {
		for _, id := range g.Members {
			var names []string
			for _, r := range st.Holds[id] {
				if n := v.Roles[r]; n != "" {
					names = append(names, n)
				}
			}
			v.Members[id] = names
		}
	}
	return v, true
}

// Communities lists the community servers of the communities this
// identity is in.
func (e *Engine) Communities() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for s, cm := range e.st.Communities {
		if !cm.Left {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// RoleByName finds a role by the name this device can open, in any case.
func (v CommunityView) RoleByName(name string) (uint64, bool) {
	for id, n := range v.Roles {
		if strings.EqualFold(n, name) {
			return id, true
		}
	}
	return 0, false
}

// Can reports whether this identity has a permission.
func (v CommunityView) Can(perm string) bool { return slices.Contains(v.Perms, perm) }
