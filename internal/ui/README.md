# Railcar's interface

This page describes the `ui` package: Railcar's screens, its keys, and the `Backend` interface that connects it to the protocol. The interface uses Bubble Tea, Bubbles, Lip Gloss, and Huh, all at version 2. `railcar` runs it with `internal/app`, which implements `Backend` with the protocol client and the chat layer.

To try it without a homeserver, run the demo. It has made-up chats and doesn't use the network:

```sh
go run ./cmd/railcar --demo
```

## Screens

1. Onboarding. A Huh form offers three choices: **Create an identity**, **Sign in with another device**, or **Sign in with your recovery code**.
   - Creating an identity asks for the homeserver, the handle, and the registration code, and checks the handle syntax (§5.4). Then it shows the 12 recovery words once, with a warning to write them down. After **I've written them down**, it asks for 3 of the words, chosen at random by position, as §6.1 asks of clients. Answers match when they're the same after trimming, in any case. Esc goes back to the words.
   - Nothing is registered until the words are typed back. If the homeserver then refuses the handle or the registration code, the form comes back with the error, and the words stay the same: after fixing it, the identity registers without asking for the words again.
   - Signing in with another device (§7.2) offers two ways. **Show a code here for the other device to approve** asks for the handle and homeserver, then shows the link code as a QR code, drawn with half-block characters, and as text, with a field for the confirmation code the other device shows. **Enter an approval code from the other device** takes an approval code instead (§7.2.1). Either way, Railcar tries to sign in every 2 seconds until the other device has added it, then opens the main view, where the chats arrive as the other devices add this one.
   - Signing in with the recovery code, for someone who has lost every device, asks for the handle, the homeserver, and the 12 words. It adds this device and removes every other one (§6.4.1). Contacts then add the new device to their chats with the identity (§11.9), and the chats come back without their earlier messages.
2. The main view. It has three panes and a status bar:
   - The chat list: direct chats, group chats, and contact requests, newest activity first, then each community under a heading, with its channels in their order: `#` for text channels, and a speaker for voice channels, with how many people are in them. Enter on a heading folds the community's channels away. Unread counts show on each chat, and on a folded community. Press / to find a chat by its title or a member's name or handle. Contact requests are in the list too: someone else's, until it's accepted or declined, and the identity's own, until the chat starts.
   - The open chat. Messages are grouped by day, and by sender within a day, with a time on each group. Replies show the message they answer, edited messages say so, deleted ones say **Message deleted**, and reactions show under the message with their counts. The line under the chat's title says who's typing. Lines Railcar shows on its own, such as "@kat has a new device", are centered, without a sender (§11.9). Below the messages is the composer.
   - An open contact request shows who it's from, with a to accept it and d to decline it.
   - The details pane, opened with Ctrl+O. In a direct chat, it shows the other person's public profile (§13): their initials in place of a picture, which terminals can't show well, their display name and handle, their presence and status message, their bio, and their links, each with its full domain first. Elsewhere it lists the chat's members, with their presence, and handles, a group chat's admins and topic and the add-requests waiting for an admin, and a channel's community and its members' roles.
   - A voice channel shows who's in it, and how to join it.
   - While there's a call or a voice channel, the call bar sits above the status bar: who's calling or who's in it, how long it has lasted, whether you're muted or deafened, a level meter for your microphone, who's muted and who's speaking, and `direct` when the call may connect directly.
   - The status bar shows the identity's handle, its homeserver, its presence when it isn't **Online**, and the stream's state: **Connected**, **Connecting** or **Reconnecting** with a spinner, or **Offline**.

   Under 80 columns, the main view shows one pane at a time: the chat list, the open chat, or its details.
3. Overlays. Press ? for every key, and Ctrl+P or : for the command palette. The palette finds commands and chats as you type, and runs commands by name, such as `:quit` and `:sign-out`. Signing out asks first.
   - Ctrl+N, or `:add`, opens **Add a contact**, which takes someone's code: `@handle::code`, or its link (§9.4). `:add @ravi.example.com::Kx82mPq1Za` fills it in.
   - `:remove-contact` and `:block` remove or block someone, after asking: the person in the open direct chat or contact request, or `@handle` (§9.6). The direct chat ends, with its messages kept. A blocked person isn't told, and their requests are dropped. `:unblock @handle` unblocks someone, and `:blocked` lists who's blocked.
   - `:invite` makes an invite code, shows it, and copies it to the clipboard where the terminal allows that (§9.1).
   - `:add-device` opens **Add a device**, which takes a new device's link code and a name for it. It then shows the warning §7.2 requires and the confirmation code to type on the new device, and waits for it. `:add-device link-…` fills in the code.
   - `:approval-code` opens **Show an approval code**: the warning first, then the code as a QR code and as text, for 10 minutes and one device (§7.2.1).
   - `:devices` opens **Your devices**: each device's name and the date it was added. d removes the selected one, after asking, and a adds one.
   - `:new-group` opens **New group chat**: a name, then the contacts to put in it, chosen with Space (§11.5).
   - In a group chat: `:add-member @handle` adds someone, directly as an admin, or by asking the admins otherwise; `:approve` and `:decline-add` answer the oldest add-request; `:remove-member`, `:make-admin`, and `:revoke-admin` take a handle; `:rename` and `:topic` take the text; `:group-picture` takes the path to a picture of at most 256 KiB, or `none`, and `:save-picture` saves it, since a terminal can't show it; `:move-hub` moves the hub to your homeserver, `:recreate` re-creates a stuck group (or, in a direct chat, the chat, which carries on as the same conversation), and `:switch` moves to a group someone re-created; and `:leave` leaves (§11.7, §11.8, §11.11).
   - `:create-community` opens **Create a community**: the community server, a name, and the server's creation code. `:join` followed by a code joins one (§16.6).
   - In a channel: `:code` makes a join code and copies it; `:history` reads the channel's history; `:add-channel` and `:add-voice-channel` take a name; `:add-role Name talk moderate` adds a role; `:give-role @handle Name` assigns one; `:kick`, `:ban`, and `:timeout @handle minutes` moderate; `:approval on` makes joins need a moderator's approval, and `:join-requests`, `:approve-join @handle`, and `:decline-join @handle` answer requests; `:move-community d.example.org CODE` moves the community to another community server, as its owner, with that server's creation code if it needs one (§16.6); `:undo`, as the owner or an administrator, undoes a member's change that broke the community's rules and stopped it (§16.5); and `:leave-community` leaves.
   - `:proxy` says whether connections go through a proxy, and which, with its password hidden, and whether calls are allowed with it. The proxy is set with `--proxy` or in `railcar.conf`.
   - `:presence away Back at 3` sets the presence state, one of available, online, away, busy, or offline, and optionally a status message; `:status` alone sets or clears the status message (§13.2). After 10 minutes without a key or a click, Railcar sets an available or online identity away, and back at the next key.
   - `:name`, `:bio`, `:links`, and `:avatar` change the public profile, which anyone can see (§13.1). `:avatar` takes the path to a PNG, JPEG, WebP, or GIF of at most 1 MB, or `none`. `:profile` shows it.
   - Ctrl+A, or `:attach`, opens **Send a file**, a file picker that starts in the home folder; `:attach ~/Pictures/map.png` sends a file by its path (§14). Messages show their files with a name, a size, and an image's size in pixels when Railcar has its bytes; audio files show as voice messages, since a terminal can't play them. `:save` downloads the latest message's files to the Downloads folder, or `:save ~/somewhere` to another one. A file past its 30 days shows "This file has expired".
   - Read receipts (§13.3): `:ask-receipts on` makes your messages ask for them; it's off until you turn it on. The first time someone asks in a chat, **Send read receipts to Kat?** opens: y sends them in that chat, n doesn't, and Esc asks again the next time Railcar starts. `:receipts on` or `:receipts off` changes the answer later. Your messages show when they've been read.
   - `:move chat.example.org kat CODE` moves the identity to another homeserver, with the name to use there and its registration code (§6.6). The handle becomes the name under that server's handle domain, and contacts follow.
   - `:backup-on` opens **Turn on the backup**, which takes a passphrase of six or more random words and shows it as dots (§7.5). `:backup-now` uploads what's new, `:backup` shows the state, `:backup-off` turns it off, and `:restore-backup` restores it, asking for the passphrase when this device needs it.
   - A message that mentions you, or an `@everyone` that counts, says **Mentions you**, rings the terminal bell, and shows a desktop notification: OSC 9 in iTerm2, WezTerm, ghostty, kitty, and Windows Terminal, OSC 777 in foot, urxvt, and VTE-based terminals, and otherwise `notify-send` on Linux or `osascript` on macOS when they're there. It names who and where, never the text. `:notifications` turns them off or on for this run. Typing `@` and a handle in a message mentions that person.
   - `:report spam` reports the latest message from someone else, with the reason (§17.2). Railcar first shows the message, the reason, and who can read the report: the homeserver's admins, or for a channel the community's moderators and the community server. Enter sends it, and f also sends it to the sender's homeserver.
   - Calls (§15.2): `:call` calls the open direct chat or group chat. A ring rings the terminal bell and shows a desktop notification, as mentions do; Ctrl+Y or `:answer` answers, and Ctrl+X or `:decline-call` declines. In a call, Ctrl+R mutes or unmutes, Ctrl+X hangs up, and `:mute`, `:unmute`, `:deafen`, `:undeafen`, and `:hangup` work too. Ctrl+Y during a call ends it and answers the one waiting. `:direct-calls on` lets calls and voice channels connect directly, which shows your IP address; they're relayed otherwise.
   - `:audio-devices` opens **Audio devices**, which lists the microphones and speakers connected now, with **System default** first. Enter chooses one, for the calls and voice channels joined after that, and the profile keeps the choice. A chosen device that isn't connected stays in the list, and calls use the default until it's back.
   - Voice channels (§16.4): `:join-voice` in a voice channel joins it, and Ctrl+X or `:leave-voice` leaves; Ctrl+R, `:mute`, and `:deafen` work there too.
   - `:reply`, `:edit`, `:delete`, and `:react` act on the latest message: someone else's for replying and reacting, the identity's own for editing and deleting. `:react 🎉` picks the emoji; it's 👍 otherwise, and reacting again with the same emoji takes it back.

The mouse works too: click a chat to open it, click a pane to move to it, and scroll the messages or the chat list.

## Keys

| Key | Where | What it does |
|---|---|---|
| Tab, Shift+Tab | Anywhere | Move to the next or previous pane |
| Alt+Down, Alt+Up | Anywhere | Open the next or previous chat |
| Ctrl+O | Anywhere | Show or hide the details pane |
| Ctrl+P | Anywhere | Open the command palette |
| Ctrl+N | Anywhere | Add a contact |
| Ctrl+A | Anywhere | Send a file to the open chat |
| Ctrl+Y | Anywhere | Answer a call that's ringing |
| Ctrl+X | Anywhere | Hang up, decline a ring, or leave a voice channel |
| Ctrl+R | Anywhere | Mute or unmute in a call or a voice channel |
| Ctrl+C | Anywhere | Quit |
| ?, : | Outside the composer | Show help, or open the command palette |
| Up, Down, Enter, / | The chat list | Move, open a chat, find a chat |
| a, d | A contact request | Accept or decline it |
| Up, Down, PgUp, PgDn, Home, End | The messages | Scroll |
| r, e | The messages | Reply to the latest message, or edit your own latest one |
| Enter | The composer | Send |
| Shift+Enter, Alt+Enter, Ctrl+J | The composer | Start a new line |
| Esc | Anywhere | Go back: out of replying or editing, from the composer to the messages, then to the chat list |

Shift+Enter needs a terminal that reports it, such as one with the kitty keyboard protocol. Alt+Enter and Ctrl+J work in the others.

## Look

The dark theme is Reroute's graphite, with Reroute orange (`#ff4a1c`) for focus and unread counts. On a light terminal, the interface uses a light theme with the same roles. Corners are square. With `NO_COLOR` set, Bubble Tea leaves out color, and the interface marks focus and selection with thick borders and reversed text, so nothing depends on color alone.

## The Backend interface

The interface reaches the protocol only through `Backend`, in `backend.go`. It calls every method from a Bubble Tea command, so methods may block on the network, and must be safe to call from more than one goroutine.

| Method | What it does |
|---|---|
| `Open` | Loads the profile's identity, or returns `ErrNoIdentity` to start onboarding |
| `SignUp` | Starts an identity with a homeserver, a handle, and a registration code, and returns its 12 recovery words |
| `FinishSignUp` | Called after the user has typed back the words: registers the identity, and forgets the words. `ErrSignUpRefused` sends the user back to the form |
| `Recover` | Signs in to an existing identity with its handle, homeserver, and recovery words, removing its other devices |
| `StartLink`, `StartLinkWithCode` | Start signing in with another device: show a link code, or use an approval code |
| `ConfirmLink` | Sends back the confirmation code the other device shows |
| `FinishLink` | Signs in once the other device has added this one, or returns `ErrNotLinkedYet` |
| `CancelLink` | Gives up on signing in with another device |
| `ApproveLinkCode`, `ShowApprovalCode` | Start adding a new device: by its link code, which returns the confirmation code, or by an approval code |
| `FinishApproval` | Adds the new device once it has answered, or returns false while it hasn't |
| `CancelApproval` | Stops adding a device, and forgets its code |
| `Devices`, `RemoveDevice` | List the identity's devices, and remove one (§7.6) |
| `AudioDevices`, `SetAudioDevices` | List the microphones and speakers, and choose them for calls and voice channels |
| `Connect` | Starts the live stream, which reconnects on its own |
| `Events` | A channel of `StreamChanged`, `ChatChanged`, `ChatRemoved`, `MessageAdded`, `MessageChanged`, `Typing`, `IdentityChanged`, `CallChanged`, and `SignedOut` events. A change of someone's presence or profile comes as a `ChatChanged` for each chat they're in |
| `Chats` | Every direct chat and group chat, and every contact request, as a `ContactRequest` or `SentRequest` |
| `Messages` | Up to a limit of a chat's messages, oldest first, before a given message or the latest |
| `Send` | Queues a message and returns it as sending; a `MessageChanged` event follows when it's sent or fails |
| `Edit`, `Delete`, `React` | Change one of the identity's own messages, delete it for everyone, or react to any message (§12) |
| `SetTyping` | Sends a typing indicator (§13.3) |
| `MarkRead` | Clears a chat's unread count |
| `MakeInvite` | Makes an invite code, as `@handle::code` |
| `AddContact` | Sends a contact request with someone's code |
| `AcceptRequest`, `DeclineRequest` | Answer a contact request; accepting starts the direct chat |
| `Contacts` | Lists the contacts, for choosing who goes in a group chat |
| `CreateGroup` | Creates a group chat with some contacts, by handle |
| `Act` | Runs an `Action`, such as `add-member`, `join-community`, `kick`, `read-receipts`, `call`, or `join-voice`, and returns a line to show |
| `SignOut` | Removes the identity from this device |
| `Close` | Stops the stream and closes `Events` |

Typing indicators follow §13.3: the composer sends one at most once a second while you type, and a stop after 7 seconds without a keystroke or when you send. Someone else's disappears after 10 seconds without news.

`DemoBackend`, in `demo.go`, `demo_contacts.go`, `demo_groups.go`, and `demo_calls.go`, implements it with made-up chats and a community, Night shift, with two text channels and a voice channel. A contact added in the demo accepts a few seconds later, and Ravi calls a few seconds after it connects, to show the call bar; calls in the demo go nowhere. `railcar --demo` runs it live, with a short wait to connect, a dropped connection every few minutes, and replies in direct chats. The tests run it with a fixed clock and no waits.

## Tests

The golden files in `testdata/` are the screen as plain text: onboarding, the recovery words, the main view at 120 by 40 and at 70 by 30, a sent message, and the details pane with a profile. After a change to the look, check the new screens and update them:

```sh
go test ./internal/ui -update
```
