package chat

import (
	"testing"

	"github.com/rerouteholdings/railcar/internal/mls"
)

// TestMembersRulesGate checks how a group's required capabilities and its
// leaves decide members_rules (§16.2).
func TestMembersRulesGate(t *testing.T) {
	on := []mls.Extension{mls.RequiredCapabilities([]uint16{ExtGroup, ExtCommunityHead, ExtMembersRules}, nil, nil)}
	off := []mls.Extension{mls.RequiredCapabilities([]uint16{ExtGroup, ExtCommunityHead}, nil, nil)}
	if !requires(on, ExtMembersRules) || requires(off, ExtMembersRules) || requires(nil, ExtMembersRules) {
		t.Error("requires")
	}
	leaf := func(caps ...uint16) mls.Member {
		var m mls.Member
		m.Capabilities.Extensions = caps
		return m
	}
	st := &mls.State{Members: []mls.Member{leaf(ExtGroup, ExtMembersRules), leaf(ExtMembersRules)}}
	if !allList(st, ExtMembersRules) {
		t.Error("every leaf lists it")
	}
	st.Members = append(st.Members, leaf(ExtGroup))
	if allList(st, ExtMembersRules) || allList(&mls.State{}, ExtMembersRules) {
		t.Error("a leaf without it, or none")
	}
	if l := (&Device{}).communityLeaf(); !contains(l.Capabilities.Extensions, ExtMembersRules) {
		t.Error("community leaves don't list 0xF0A5")
	}
}

func contains(s []uint16, t uint16) bool {
	for _, x := range s {
		if x == t {
			return true
		}
	}
	return false
}
