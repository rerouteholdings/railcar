# Railcar's interface

This page describes the `ui` package: Railcar's screens, its keys, and the `Backend` interface that connects it to the protocol. The interface uses Bubble Tea, Bubbles, Lip Gloss, and Huh, all at version 2. `railcar` runs it with `internal/app`, which implements `Backend` with the protocol client and the chat layer.

To try it without a homeserver, run the demo. It has made-up chats and doesn't use the network:

```sh
go run ./cmd/railcar --demo
```

## Screens

1. Onboarding. A Huh form offers two choices: **Create an identity**, or **Sign in with your recovery code**.
   - Creating an identity asks for the homeserver, the handle, and the registration code, and checks the handle syntax (§5.4). Then it shows the 12 recovery words once, with a warning to write them down. After **I've written them down**, it asks for 3 of the words, chosen at random by position, as §6.1 asks of clients. Answers match when they're the same after trimming, in any case. Esc goes back to the words.
   - Nothing is registered until the words are typed back. If the homeserver then refuses the handle or the registration code, the form comes back with the error, and the words stay the same: after fixing it, the identity registers without asking for the words again.
   - Signing in asks for the handle and the 12 words.
2. The main view. It has three panes and a status bar:
   - The chat list, newest activity first, with unread counts. Press / to find a chat by its title or a member's name or handle. Contact requests are in the list too: someone else's, until it's accepted or declined, and the identity's own, until the chat starts.
   - The open chat. Messages are grouped by day, and by sender within a day, with a time on each group. Replies show the message they answer, edited messages say so, deleted ones say **Message deleted**, and reactions show under the message with their counts. The line under the chat's title says who's typing. Below the messages is the composer.
   - An open contact request shows who it's from, with a to accept it and d to decline it.
   - The details pane, opened with Ctrl+O. It lists the chat's members and handles.
   - The status bar shows the identity's handle, its homeserver, and the stream's state: **Connected**, **Connecting** or **Reconnecting** with a spinner, or **Offline**.

   Under 80 columns, the main view shows one pane at a time: the chat list, the open chat, or its details.
3. Overlays. Press ? for every key, and Ctrl+P or : for the command palette. The palette finds commands and chats as you type, and runs commands by name, such as `:quit` and `:sign-out`. Signing out asks first.
   - Ctrl+N, or `:add`, opens **Add a contact**, which takes someone's code: `@handle::code`, or its link (§9.4). `:add @ravi.example.com::Kx82mPq1Za` fills it in.
   - `:invite` makes an invite code, shows it, and copies it to the clipboard where the terminal allows that (§9.1).
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
| `Recover` | Signs in to an existing identity with its handle and recovery words |
| `Connect` | Starts the live stream, which reconnects on its own |
| `Events` | A channel of `StreamChanged`, `ChatChanged`, `ChatRemoved`, `MessageAdded`, `MessageChanged`, `Typing`, and `SignedOut` events |
| `Chats` | Every direct chat and group chat, and every contact request, as a `ContactRequest` or `SentRequest` |
| `Messages` | Up to a limit of a chat's messages, oldest first, before a given message or the latest |
| `Send` | Queues a message and returns it as sending; a `MessageChanged` event follows when it's sent or fails |
| `Edit`, `Delete`, `React` | Change one of the identity's own messages, delete it for everyone, or react to any message (§12) |
| `SetTyping` | Sends a typing indicator (§13.3) |
| `MarkRead` | Clears a chat's unread count |
| `MakeInvite` | Makes an invite code, as `@handle::code` |
| `AddContact` | Sends a contact request with someone's code |
| `AcceptRequest`, `DeclineRequest` | Answer a contact request; accepting starts the direct chat |
| `SignOut` | Removes the identity from this device |
| `Close` | Stops the stream and closes `Events` |

Typing indicators follow §13.3: the composer sends one at most once a second while you type, and a stop after 7 seconds without a keystroke or when you send. Someone else's disappears after 10 seconds without news.

`DemoBackend`, in `demo.go` and `demo_contacts.go`, implements it with made-up chats. A contact added in the demo accepts a few seconds later. `railcar --demo` runs it live, with a short wait to connect, a dropped connection every few minutes, and replies in direct chats. The tests run it with a fixed clock and no waits.

## Tests

The golden files in `testdata/` are the screen as plain text: onboarding, the recovery words, the main view at 120 by 40 and at 70 by 30, and a sent message. After a change to the look, check the new screens and update them:

```sh
go test ./internal/ui -update
```
