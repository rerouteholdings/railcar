package chat

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// TypeAppMessage is the type tag of the signed plaintext inside every MLS
// application message (§12).
const TypeAppMessage = "app-message"

// AppMessage is the body of an app-message (§12).
type AppMessage struct {
	ID      string         `json:"id"`    // the sender's identifier
	Group   wire.Bytes     `json:"group"` // the group it was sent in
	Channel *uint64        `json:"channel,omitempty"`
	TS      int64          `json:"ts"`
	C       jsontext.Value `json:"c"`
}

// Content types this client knows (§12). Every other t is unknown
// content, shown as an "unsupported message" placeholder.
const (
	CText           = "text"
	CEdit           = "edit"
	CDelete         = "delete"
	CReact          = "react"
	CFile           = "file"
	CReceipt        = "receipt"
	CTyping         = "typing"
	CGroupMeta      = "group-meta"
	CCall           = "call"
	CVerifyCommit   = "verify-commit"
	CVerifyReveal   = "verify-reveal"
	CVerifyInPerson = "verify-in-person"
	CAddRequest     = "add-request"
	CAddAnswer      = "add-answer"
	CVoiceKey       = "voice-key"
	CCommunityKeys  = "community-keys"
	CHistoryRequest = "history-request"
	CVoiceMute      = "voice-mute"
)

var knownContent = map[string]bool{
	CText: true, CEdit: true, CDelete: true, CReact: true, CFile: true, CReceipt: true, CTyping: true,
	CGroupMeta: true, CCall: true, CVerifyCommit: true, CVerifyReveal: true, CVerifyInPerson: true,
	CAddRequest: true, CAddAnswer: true, CVoiceKey: true, CCommunityKeys: true, CHistoryRequest: true,
	CVoiceMute: true,
}

// Text is text content (§12).
type Text struct {
	T        string           `json:"t"`
	ID       string           `json:"id"`
	TS       int64            `json:"ts"`
	Body     *string          `json:"body"`
	Reply    string           `json:"reply,omitempty"`
	Mentions []string         `json:"mentions,omitempty"`
	Everyone bool             `json:"everyone,omitempty"`
	Channels []jsontext.Value `json:"channels,omitempty"`
	Receipt  bool             `json:"receipt,omitempty"`
	Previews []jsontext.Value `json:"previews,omitempty"`
	Files    []jsontext.Value `json:"files,omitempty"`
}

// Edit replaces a message's text (§12).
type Edit struct {
	T      string  `json:"t"`
	ID     string  `json:"id"`
	TS     int64   `json:"ts"`
	Target string  `json:"target"`
	Body   *string `json:"body"`
}

// Delete deletes a message for everyone (§12).
type Delete struct {
	T      string `json:"t"`
	ID     string `json:"id"`
	TS     int64  `json:"ts"`
	Target string `json:"target"`
}

// React adds or removes a reaction (§12).
type React struct {
	T      string `json:"t"`
	ID     string `json:"id"`
	TS     int64  `json:"ts"`
	Target string `json:"target"`
	Emoji  string `json:"emoji"`
	Remove bool   `json:"remove,omitempty"`
}

// Receipt acknowledges every message up to upto that asked for one
// (§13.3).
type Receipt struct {
	T    string `json:"t"`
	TS   int64  `json:"ts"`
	Upto string `json:"upto"`
}

// Typing is a typing indicator (§13.3).
type Typing struct {
	T  string `json:"t"`
	TS int64  `json:"ts"`
	On *bool  `json:"on"`
}

// GroupMeta carries a group's whole state: its name, topic, and picture
// (§12). An absent avatar keeps the current picture, and null removes it.
type GroupMeta struct {
	T      string         `json:"t"`
	TS     int64          `json:"ts"`
	Name   *string        `json:"name,omitempty"`
	Topic  *string        `json:"topic,omitempty"`
	Avatar jsontext.Value `json:"avatar,omitempty"`
}

// GroupPicture is a group's picture, carried inline (§12).
type GroupPicture struct {
	MIME string     `json:"mime"`
	Data wire.Bytes `json:"data"`
}

// MaxGroupPicture is the largest group picture: 256 KiB (§12).
const MaxGroupPicture = 262144

// AddRequest asks the group's admins to add someone (§11.8).
type AddRequest struct {
	T   string `json:"t"`
	ID  string `json:"id"`
	TS  int64  `json:"ts"`
	Who string `json:"who"`
}

// AddAnswer answers an add-request: with an approval, or declined
// without one (§11.8).
type AddAnswer struct {
	T        string       `json:"t"`
	TS       int64        `json:"ts"`
	Target   string       `json:"target"`
	Approval *wire.Signed `json:"approval,omitempty"`
}

// signAppMessage signs content as an app-message with the message key,
// and returns JCS(signed), the MLS plaintext (§12).
func signAppMessage(key ed25519.PrivateKey, id string, group []byte, ts int64, content any) ([]byte, error) {
	c, err := wire.JCS(content)
	if err != nil {
		return nil, err
	}
	s, err := sig.SignMessageKey(key, TypeAppMessage, AppMessage{ID: id, Group: group, TS: ts, C: c})
	if err != nil {
		return nil, err
	}
	return s.Canonical(), nil
}

// openAppMessage parses an MLS plaintext as a signed app-message and
// checks its signature against the sender's message key, the key in the
// sender's leaf (§3.2.3, §12). The caller checks the rest: that the
// key's msg-key is the sender's, and the identity and group.
func openAppMessage(plaintext, msgKey []byte) (AppMessage, string, error) {
	var am AppMessage
	s, err := wire.ParseSigned(plaintext)
	if err != nil {
		return am, "", err
	}
	if s.T != TypeAppMessage {
		return am, "", fmt.Errorf("chat: a %q, not an app-message", s.T)
	}
	if err := sig.VerifyMessageKey(s, msgKey); err != nil {
		return am, "", fmt.Errorf("chat: the app-message's signature: %w", err)
	}
	if err := s.Body(&am); err != nil {
		return am, "", err
	}
	if am.ID == "" || len(am.Group) == 0 || len(am.C) == 0 {
		return am, "", errors.New("chat: an app-message without id, group, or c")
	}
	var t struct {
		T string `json:"t"`
	}
	if err := wire.Unmarshal(am.C, &t); err != nil || t.T == "" {
		return am, "", errors.New("chat: content without t")
	}
	return am, t.T, nil
}

// validMsgID checks a msg-id: 16 bytes in base64url (§4.2).
func validMsgID(s string) bool {
	_, err := wire.DecodeB64Len(s, 16)
	return err == nil
}

// parseContent decodes known content into its type and checks the fields
// the schema requires. A known t that doesn't match its schema is
// malformed, and the message is dropped (§12).
func parseContent(t string, raw jsontext.Value) (any, error) {
	bad := func(what string) (any, error) { return nil, fmt.Errorf("chat: malformed %s content: %s", t, what) }
	switch t {
	case CText:
		var c Text
		if err := wire.Unmarshal(raw, &c); err != nil {
			return bad(err.Error())
		}
		if c.Body == nil || !validMsgID(c.ID) {
			return bad("needs id and body")
		}
		return c, nil
	case CEdit:
		var c Edit
		if err := wire.Unmarshal(raw, &c); err != nil {
			return bad(err.Error())
		}
		if c.Body == nil || !validMsgID(c.ID) || !validMsgID(c.Target) {
			return bad("needs id, target, and body")
		}
		return c, nil
	case CDelete:
		var c Delete
		if err := wire.Unmarshal(raw, &c); err != nil {
			return bad(err.Error())
		}
		if !validMsgID(c.ID) || !validMsgID(c.Target) {
			return bad("needs id and target")
		}
		return c, nil
	case CReact:
		var c React
		if err := wire.Unmarshal(raw, &c); err != nil {
			return bad(err.Error())
		}
		if !validMsgID(c.ID) || !validMsgID(c.Target) || c.Emoji == "" {
			return bad("needs id, target, and emoji")
		}
		return c, nil
	case CReceipt:
		var c Receipt
		if err := wire.Unmarshal(raw, &c); err != nil {
			return bad(err.Error())
		}
		if !validMsgID(c.Upto) {
			return bad("needs upto")
		}
		return c, nil
	case CGroupMeta:
		var c GroupMeta
		if err := wire.Unmarshal(raw, &c); err != nil {
			return bad(err.Error())
		}
		if len(c.Avatar) > 0 && string(c.Avatar) != "null" {
			var pic GroupPicture
			if err := wire.Unmarshal(c.Avatar, &pic); err != nil {
				return bad("a malformed avatar")
			}
			switch pic.MIME {
			case "image/png", "image/jpeg", "image/webp", "image/gif":
			default:
				return bad("an avatar of another type")
			}
			if len(pic.Data) == 0 || len(pic.Data) > MaxGroupPicture {
				return bad("an avatar over 256 KiB")
			}
		}
		return c, nil
	case CAddRequest:
		var c AddRequest
		if err := wire.Unmarshal(raw, &c); err != nil {
			return bad(err.Error())
		}
		if !validMsgID(c.ID) || !identity.ValidIdentifier(c.Who) {
			return bad("needs id and who")
		}
		return c, nil
	case CAddAnswer:
		var c AddAnswer
		if err := wire.Unmarshal(raw, &c); err != nil {
			return bad(err.Error())
		}
		if !validMsgID(c.Target) {
			return bad("needs target")
		}
		return c, nil
	case CTyping:
		var c Typing
		if err := wire.Unmarshal(raw, &c); err != nil {
			return bad(err.Error())
		}
		if c.On == nil {
			return bad("needs on")
		}
		return c, nil
	}
	return nil, nil
}
