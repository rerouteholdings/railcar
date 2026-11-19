# Railcar's interface

This page describes the `ui` package: Railcar's screens, its keys, and the `Backend` interface that connects it to the protocol. The interface uses Bubble Tea, Bubbles, Lip Gloss, and Huh, all at version 2.

To try it without a homeserver, run the demo. It has made-up chats and doesn't use the network:

```sh
go run ./cmd/railcar --demo
```

## Screens

1. Onboarding. A Huh form offers two choices: **Create an identity**, or **Sign in with your recovery code**.
   - Creating an identity asks for the homeserver, the handle, and the registration code, and checks the handle syntax (§5.4). Then it shows the 12 recovery words once, with a warning to write them down. After **I've written them down**, it asks for 3 of the words, chosen at random by position, as §6.1 asks of clients. Answers match when they're the same after trimming, in any case. Esc goes back to the words.
   - Signing in asks for the handle and the 12 words.
2. The main view. It has three panes and a status bar:
   - The chat list, newest activity first, with unread counts. Press / to find a chat by its title or a member's name or handle.
   - The open chat. Messages are grouped by day, and by sender within a day, with a time on each group. Replies show the message they answer, and edited messages say so. Below them is the composer.
   - The details pane, opened with Ctrl+O. It lists the chat's members and handles.
   - The status bar shows the identity's handle, its homeserver, and the stream's state: **Connected**, **Connecting** or **Reconnecting** with a spinner, or **Offline**.

   Under 80 columns, the main view shows one pane at a time: the chat list, the open chat, or its details.
3. Overlays. Press ? for every key, and Ctrl+P or : for the command palette. The palette finds commands and chats as you type, and runs commands by name, such as `:quit` and `:sign-out`. Signing out asks first.

The mouse works too: click a chat to open it, click a pane to move to it, and scroll the messages or the chat list.

## Keys

| Key | Where | What it does |
|---|---|---|
| Tab, Shift+Tab | Anywhere | Move to the next or previous pane |
| Alt+Down, Alt+Up | Anywhere | Open the next or previous chat |
| Ctrl+O | Anywhere | Show or hide the details pane |
| Ctrl+P | Anywhere | Open the command palette |
| Ctrl+C | Anywhere | Quit |
| ?, : | Outside the composer | Show help, or open the command palette |
| Up, Down, Enter, / | The chat list | Move, open a chat, find a chat |
| Up, Down, PgUp, PgDn, Home, End | The messages | Scroll |
| Enter | The composer | Send |
| Shift+Enter, Alt+Enter, Ctrl+J | The composer | Start a new line |
| Esc | Anywhere | Go back: from the composer to the messages, then to the chat list |

Shift+Enter needs a terminal that reports it, such as one with the kitty keyboard protocol. Alt+Enter and Ctrl+J work in the others.

## Look

The dark theme is Reroute's graphite, with Reroute orange (`#ff4a1c`) for focus and unread counts. On a light terminal, the interface uses a light theme with the same roles. Corners are square. With `NO_COLOR` set, Bubble Tea leaves out color, and the interface marks focus and selection with thick borders and reversed text, so nothing depends on color alone.

## The Backend interface

The interface reaches the protocol only through `Backend`, in `backend.go`. It calls every method from a Bubble Tea command, so methods may block on the network, and must be safe to call from more than one goroutine.

| Method | What it does |
|---|---|
| `Open` | Loads the profile's identity, or returns `ErrNoIdentity` to start onboarding |
| `SignUp` | Creates an identity with a homeserver, a handle, and a registration code, and returns its 12 recovery words |
| `FinishSignUp` | Called after the user has typed back the words; the backend forgets them |
| `Recover` | Signs in to an existing identity with its handle and recovery words |
| `Connect` | Starts the live stream, which reconnects on its own |
| `Events` | A channel of `StreamChanged`, `ChatChanged`, `MessageAdded`, `MessageChanged`, and `SignedOut` events |
| `Chats` | Every direct chat and group chat |
| `Messages` | Up to a limit of a chat's messages, oldest first, before a given message or the latest |
| `Send` | Queues a message and returns it as sending; a `MessageChanged` event follows when it's sent or fails |
| `MarkRead` | Clears a chat's unread count |
| `SignOut` | Removes the identity from this device |
| `Close` | Stops the stream and closes `Events` |

`DemoBackend`, in `demo.go`, implements it with made-up chats. `railcar --demo` runs it live, with a short wait to connect, a dropped connection every few minutes, and replies in direct chats. The tests run it with a fixed clock and no waits.

## Tests

The golden files in `testdata/` are the screen as plain text: onboarding, the recovery words, the main view at 120 by 40 and at 70 by 30, and a sent message. After a change to the look, check the new screens and update them:

```sh
go test ./internal/ui -update
```
