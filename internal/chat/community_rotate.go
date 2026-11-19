package chat

import (
	"bytes"
	"context"
	"maps"
	"slices"

	"github.com/rerouteholdings/railcar/internal/tieroute/community"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// rotateMetaKey replaces the community metadata key after a kick or ban
// (§16.2.4): one entry re-seals every role's name and color, and every
// name and topic of the channels everyone sees, under a new key; then
// this device sends the members group the keys, the new one first. A
// removed member keeps the names it saw. It needs administrate; a
// moderator without it leaves rotating to an admin. The caller holds
// e.mu, after the Removes are committed.
func (e *Engine) rotateMetaKey(ctx context.Context, cm *Community) error {
	c, err := e.communityChain(cm)
	if err != nil {
		return err
	}
	if !c.State().Can(e.c.ID(), community.PermAdministrate) {
		return nil
	}
	key := randKey()
	old := e.metaKeys(cm)
	err = e.appendEntry(ctx, cm, func(st *community.State, seq uint64) ([]map[string]any, error) {
		var ops []map[string]any
		reseal := func(op map[string]any, s *community.Sealed, sseq uint64, what string, id uint64, field string) error {
			if s == nil || bytes.Equal(s.Key, community.KeyHash(key)) {
				return nil
			}
			v, ok := community.Open(old, s, cm.ID, sseq, what, id, field)
			if !ok {
				return nil // a value this device can't open stays as it is
			}
			n, err := community.Seal(key, cm.ID, seq, what, id, field, v)
			if err != nil {
				return err
			}
			op[field] = n
			return nil
		}
		for _, id := range slices.Sorted(maps.Keys(st.Roles)) {
			r := st.Roles[id]
			if id == 0 {
				continue
			}
			op := map[string]any{"op": community.OpEditRole, "id": id}
			if err := reseal(op, r.Name, r.NameSeq, community.WhatRole, id, community.FieldName); err != nil {
				return nil, err
			}
			if err := reseal(op, r.Color, r.ColorSeq, community.WhatRole, id, community.FieldColor); err != nil {
				return nil, err
			}
			if len(op) > 2 {
				ops = append(ops, op)
			}
		}
		for _, ch := range st.OrderedChannels() {
			if ch.Restricted() {
				continue // under its own key (§16.2.4)
			}
			op := map[string]any{"op": community.OpEditChannel, "id": ch.ID}
			if err := reseal(op, ch.Name, ch.NameSeq, community.WhatChannel, ch.ID, community.FieldName); err != nil {
				return nil, err
			}
			if err := reseal(op, ch.Topic, ch.TopicSeq, community.WhatChannel, ch.ID, community.FieldTopic); err != nil {
				return nil, err
			}
			if len(op) > 2 {
				ops = append(ops, op)
			}
		}
		if len(ops) == 0 {
			return nil, errNothingToSeal
		}
		return ops, nil
	})
	if err == errNothingToSeal {
		err = nil
	}
	if err != nil {
		return err
	}
	cm.MetaKeys = append([]wire.Bytes{key}, cm.MetaKeys...)
	if err := e.save(); err != nil {
		return err
	}
	e.refreshChannels(cm)
	e.sendKeys(ctx, cm, cm.Groups[cm.MembersGroup])
	return nil
}

// errNothingToSeal ends a rotation with no names to re-seal: the new key
// still goes to the members.
var errNothingToSeal = errorString("chat: nothing to re-seal")

type errorString string

func (e errorString) Error() string { return string(e) }
