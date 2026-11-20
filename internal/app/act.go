package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rerouteholdings/railcar/internal/chat"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/community"
	"github.com/rerouteholdings/railcar/internal/ui"
)

// channelChat fills in a community channel as the interface shows it.
func channelChat(eng *chat.Engine, c chat.Chat, out ui.Chat) ui.Chat {
	out.Kind = ui.ChannelChat
	if c.Voice {
		out.Kind = ui.VoiceChannel
	}
	out.Title = c.Name
	if out.Title == "" {
		out.Title = "Channel " + strconv.FormatUint(c.Channel, 10) + " (name not readable yet)"
	}
	out.Topic, out.Community, out.CommunityServer, out.Order = c.Topic, c.CommunityName, c.Community, c.Order
	v, _ := eng.Community(c.Community)
	me := eng.Client().ID()
	for _, id := range c.Members {
		if id == me {
			continue
		}
		m := member(eng, id)
		m.Roles = strings.Join(v.Members[id], ", ")
		out.Members = append(out.Members, m)
	}
	for _, id := range c.InVoice {
		out.InVoice = append(out.InVoice, member(eng, id))
	}
	out.Admin = v.Can(community.PermAdministrate)
	return out
}

// Contacts implements ui.Backend.
func (b *Backend) Contacts(context.Context) ([]ui.Member, error) {
	eng, err := b.engine()
	if err != nil {
		return nil, err
	}
	var out []ui.Member
	for _, c := range eng.Contacts() {
		if c.State == chat.StateContact {
			out = append(out, ui.Member{Handle: c.Handle})
		}
	}
	return out, nil
}

// idOf finds an identity by handle among the contacts and the chats'
// members.
func idOf(eng *chat.Engine, handle string) (string, error) {
	h := strings.TrimPrefix(strings.TrimSpace(handle), "@")
	for _, c := range eng.Contacts() {
		if c.Handle == h {
			return c.ID, nil
		}
	}
	for _, ch := range eng.Chats() {
		for _, id := range ch.Members {
			if eng.Handle(id) == h {
				return id, nil
			}
		}
	}
	for _, s := range eng.Communities() {
		v, _ := eng.Community(s)
		for id := range v.Members {
			if eng.Handle(id) == h {
				return id, nil
			}
		}
	}
	return "", fmt.Errorf("Railcar doesn't know @%s", h)
}

// CreateGroup implements ui.Backend.
func (b *Backend) CreateGroup(ctx context.Context, name string, handles []string) (string, error) {
	eng, err := b.engine()
	if err != nil {
		return "", err
	}
	var ids []string
	for _, h := range handles {
		id, err := idOf(eng, h)
		if err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	c, err := eng.CreateGroup(ctx, name, ids)
	var miss *chat.MissingError
	if errors.As(err, &miss) {
		return c.ID, nil
	}
	return c.ID, friendly(err)
}

// Act implements ui.Backend.
func (b *Backend) Act(ctx context.Context, chatID string, a ui.Action) (string, error) {
	eng, err := b.engine()
	if err != nil {
		return "", err
	}
	msg, err := act(ctx, eng, chatID, a)
	return msg, friendly(err)
}

func act(ctx context.Context, eng *chat.Engine, chatID string, a ui.Action) (string, error) {
	if msg, ok, err := actPeople(ctx, eng, a); ok {
		return msg, err
	}
	if msg, ok, err := actFiles(ctx, eng, chatID, a); ok {
		return msg, err
	}
	if msg, ok, err := actReceipts(ctx, eng, chatID, a); ok {
		return msg, err
	}
	if msg, ok, err := actBackup(ctx, eng, a); ok {
		return msg, err
	}
	if msg, ok, err := actContacts(ctx, eng, chatID, a); ok {
		return msg, err
	}
	if msg, ok, err := actCalls(ctx, eng, chatID, a); ok {
		if err != nil {
			return "", err
		}
		return msg, nil
	}
	if a.Kind == "move" {
		if a.Arg == "" || a.Arg2 == "" {
			return "", errors.New("type :move, the new homeserver, your name there, and its registration code if it needs one")
		}
		if err := eng.Move(ctx, a.Arg, a.Arg2, a.Arg3); err != nil {
			return "", err
		}
		return "You moved to " + a.Arg + ". You're @" + eng.Client().Handle() + " now", nil
	}
	if a.Kind == "report" {
		if err := eng.Report(ctx, chatID, []string{a.Arg2}, a.Arg, a.Arg3 == "forward"); err != nil {
			return "", err
		}
		return "Reported", nil
	}
	c, _ := eng.Chat(chatID)
	server := c.Community
	who := func() (string, error) {
		if a.Arg == "" {
			return "", errors.New("name someone, as @handle")
		}
		return idOf(eng, a.Arg)
	}
	needCommunity := func() error {
		if server == "" {
			return errors.New("open a channel of the community first")
		}
		return nil
	}
	switch a.Kind {
	case "add-member":
		id, err := who()
		if err != nil {
			return "", err
		}
		added, err := eng.AddMember(ctx, chatID, id)
		if err != nil {
			return "", err
		}
		if !added {
			return "You asked the admins to add " + a.Arg, nil
		}
		return "Added " + a.Arg, nil
	case "remove-member":
		id, err := who()
		if err != nil {
			return "", err
		}
		return "Removed " + a.Arg, eng.RemoveMember(ctx, chatID, id)
	case "make-admin", "revoke-admin":
		id, err := who()
		if err != nil {
			return "", err
		}
		on := a.Kind == "make-admin"
		if err := eng.SetAdmin(ctx, chatID, id, on); err != nil {
			return "", err
		}
		if on {
			return a.Arg + " is an admin now", nil
		}
		return a.Arg + " is no longer an admin", nil
	case "approve-add", "decline-add":
		req := a.Arg
		if req == "" {
			if len(c.Requests) == 0 {
				return "", errors.New("there's no add-request to answer")
			}
			req = c.Requests[0].ID
		}
		approve := a.Kind == "approve-add"
		if err := eng.AnswerAdd(ctx, chatID, req, approve); err != nil {
			return "", err
		}
		if approve {
			return "Approved. The member who asked adds them now", nil
		}
		return "Declined", nil
	case "leave":
		return "You left the group", eng.Leave(ctx, chatID)
	case "rename":
		return "Renamed", eng.SetGroupMeta(ctx, chatID, &a.Arg, nil)
	case "topic":
		return "Topic changed", eng.SetGroupMeta(ctx, chatID, nil, &a.Arg)
	case "move-hub":
		if err := eng.MoveHub(ctx, chatID); err != nil {
			return "", err
		}
		return "The group's hub is your homeserver now", nil
	case "recreate":
		if c.Kind == chat.KindDirect {
			if _, err := eng.RecreateDirect(ctx, chatID); err != nil {
				return "", err
			}
			return "Re-created the chat. Its history carries on", nil
		}
		_, err := eng.RecreateGroup(ctx, chatID)
		var miss *chat.MissingError
		if err != nil && !errors.As(err, &miss) {
			return "", err
		}
		msg := "Re-created the group. Its history came along"
		if miss != nil {
			msg += ". Left out, with no key package: " + strings.Join(miss.People, ", ")
		}
		return msg, nil
	case "switch":
		if err := eng.SwitchGroup(ctx, chatID); err != nil {
			return "", err
		}
		return "Switched. This group carries on the chat", nil
	case "group-picture":
		path := strings.TrimSpace(a.Arg)
		if path == "" {
			return "", errors.New("name a picture, as :group-picture ~/Pictures/trail.png, or none")
		}
		if strings.EqualFold(path, "none") {
			return "Removed the group's picture", eng.SetGroupPicture(ctx, chatID, nil)
		}
		data, err := os.ReadFile(expandHome(path))
		if err != nil {
			return "", fmt.Errorf("Railcar can't read %s", path)
		}
		return "Changed the group's picture", eng.SetGroupPicture(ctx, chatID, data)
	case "save-picture":
		dir := downloads()
		if d := strings.TrimSpace(a.Arg); d != "" {
			dir = expandHome(d)
		}
		path, err := eng.SaveGroupPicture(chatID, dir)
		if err != nil {
			return "", err
		}
		return "Saved the group's picture to " + path, nil
	case "create-community":
		if _, err := eng.CreateCommunity(ctx, strings.TrimSpace(a.Arg), a.Arg2, "", strings.TrimSpace(a.Arg3)); err != nil {
			return "", err
		}
		return "Created " + a.Arg2 + " on " + a.Arg, nil
	case "join-community":
		s, err := eng.JoinCommunity(ctx, a.Arg)
		if errors.Is(err, chat.ErrAskedToJoin) {
			return "You asked to join " + s + ". You're in once a moderator approves", nil
		}
		if err != nil {
			return "", err
		}
		v, _ := eng.Community(s)
		msg := "Joined " + v.Name + ". Channel names and history arrive once a member's app sends the keys"
		if v.Public {
			msg += ". Anyone with a code can join this community and read it, so its encryption protects against the community server, not against members"
		}
		return msg, nil
	}
	if err := needCommunity(); err != nil {
		return "", err
	}
	switch a.Kind {
	case "join-requests":
		ids, err := eng.JoinRequests(ctx, server)
		if err != nil {
			return "", err
		}
		if len(ids) == 0 {
			return "Nobody is asking to join", nil
		}
		names := make([]string, len(ids))
		for i, id := range ids {
			names[i] = "@" + eng.Handle(id)
		}
		return "Asking to join: " + strings.Join(names, ", "), nil
	case "approve-join", "decline-join":
		id, err := who()
		if err != nil {
			return "", err
		}
		if a.Kind == "decline-join" {
			return "Declined", eng.DeclineJoin(ctx, server, id)
		}
		if err := eng.ApproveJoin(ctx, server, id); err != nil {
			return "", err
		}
		return "Approved. They're in", nil
	case "approval":
		on := strings.EqualFold(strings.TrimSpace(a.Arg), "on")
		if !on && !strings.EqualFold(strings.TrimSpace(a.Arg), "off") {
			return "", errors.New("type :approval on or :approval off")
		}
		if err := eng.SetApproval(ctx, server, on); err != nil {
			return "", err
		}
		if on {
			return "Joins need a moderator's approval now", nil
		}
		return "Codes let people join without approval now", nil
	case "make-code":
		return eng.MakeCode(ctx, server, 7, 25)
	case "add-channel", "add-voice-channel":
		_, err := eng.AddChannel(ctx, server, a.Arg, "", chat.ChannelOptions{Voice: a.Kind == "add-voice-channel"})
		return "Added #" + a.Arg, err
	case "add-role":
		perms := strings.Fields(a.Arg2)
		if len(perms) == 0 {
			perms = []string{community.PermTalk}
		}
		_, err := eng.AddRole(ctx, server, a.Arg, "", perms)
		return "Added the role " + a.Arg, err
	case "give-role":
		id, err := who()
		if err != nil {
			return "", err
		}
		v, _ := eng.Community(server)
		role, ok := v.RoleByName(a.Arg2)
		if !ok {
			return "", fmt.Errorf("there's no role called %s", a.Arg2)
		}
		return "Gave " + a.Arg + " the role " + a.Arg2, eng.SetRoles(ctx, server, id, []uint64{role})
	case "history":
		r, err := eng.FetchHistory(ctx, chatID)
		if err != nil {
			return "", err
		}
		if len(r.Gaps) > 0 {
			return fmt.Sprintf("Read %d messages from history. Some are still missing; Railcar asked the members for them", r.Added), nil
		}
		return fmt.Sprintf("Read %d messages from history", r.Added), nil
	case "kick", "ban":
		id, err := who()
		if err != nil {
			return "", err
		}
		if a.Kind == "kick" {
			return "Kicked " + a.Arg, eng.Kick(ctx, server, id)
		}
		return "Banned " + a.Arg, eng.Ban(ctx, server, id)
	case "timeout":
		id, err := who()
		if err != nil {
			return "", err
		}
		mins, err := strconv.Atoi(strings.TrimSpace(a.Arg2))
		if err != nil || mins < 0 || mins > 7*24*60 {
			mins = 10
		}
		return fmt.Sprintf("Timed out %s for %d minutes", a.Arg, mins), eng.Timeout(ctx, server, id, time.Duration(mins)*time.Minute)
	case "leave-community":
		return "You left the community", eng.LeaveCommunity(ctx, server)
	}
	return "", fmt.Errorf("Railcar doesn't know the action %q", a.Kind)
}

// actReceipts runs the read receipt actions (§13.3), and reports whether
// a.Kind was one of them.
func actReceipts(ctx context.Context, eng *chat.Engine, chatID string, a ui.Action) (string, bool, error) {
	if a.Kind != "read-receipts" && a.Kind != "ask-receipts" {
		return "", false, nil
	}
	var on bool
	switch strings.ToLower(strings.TrimSpace(a.Arg)) {
	case "on", "yes":
		on = true
	case "off", "no":
	default:
		return "", true, errors.New("type on or off")
	}
	if a.Kind == "ask-receipts" {
		if err := eng.SetAskReceipts(on); err != nil {
			return "", true, err
		}
		if on {
			return "Your messages ask for read receipts now", true, nil
		}
		return "Your messages don't ask for read receipts now", true, nil
	}
	if chatID == "" {
		return "", true, errors.New("open a chat first")
	}
	if err := eng.SetReadReceipts(ctx, chatID, on); err != nil {
		return "", true, err
	}
	if on {
		return "Railcar sends read receipts in this chat to those who ask", true, nil
	}
	return "Railcar doesn't send read receipts in this chat", true, nil
}

// actBackup runs the backup actions (§7.5), and reports whether a.Kind
// was one of them.
func actBackup(ctx context.Context, eng *chat.Engine, a ui.Action) (string, bool, error) {
	switch a.Kind {
	case "backup-on":
		if err := eng.EnableBackup(ctx, a.Arg); err != nil {
			return "", true, err
		}
		return "The backup is on, and this device writes it", true, nil
	case "backup-now":
		if err := eng.BackupNow(ctx); err != nil {
			return "", true, err
		}
		return "The backup has everything", true, nil
	case "backup-off":
		if err := eng.DisableBackup(ctx); err != nil {
			return "", true, err
		}
		return "The backup is off. Your passphrase no longer opens anything", true, nil
	case "backup-status":
		info, err := eng.Backup(ctx)
		if err != nil {
			return "", true, err
		}
		switch {
		case !info.On:
			return "The backup is off", true, nil
		case info.This:
			return fmt.Sprintf("The backup is on, and this device writes it: %d segments stored", info.Stored), true, nil
		default:
			return fmt.Sprintf("The backup is on, and another of your devices writes it: %d segments stored", info.Stored), true, nil
		}
	case "restore-backup":
		if len(eng.Client().AccountKey()) != 16 && a.Arg == "" {
			return "", true, ui.ErrPassphraseNeeded
		}
		res, err := eng.RestoreBackup(ctx, a.Arg)
		if err != nil {
			if errors.Is(err, client.ErrNoPassphraseWrap) {
				return "", true, errors.New("that passphrase doesn't open the backup")
			}
			return "", true, err
		}
		msg := fmt.Sprintf("Restored %d messages. They show in their chats once this device is in them", res.Messages)
		if res.Missing {
			msg += ". Warning: your homeserver lost or withheld part of the backup"
		}
		return msg, true, nil
	}
	return "", false, nil
}
