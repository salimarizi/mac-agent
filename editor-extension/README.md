# Mac Remote Terminals

Lets the Mac Remote agent list this editor's terminals, mirror their output (with
colours) and type commands into them, so they show up on the phone's Terminals
screen next to Terminal.app and iTerm tabs.

Works in VS Code and VS Code-based editors (Cursor, Antigravity). Each editor
window serves its terminals on a random port on 127.0.0.1, protected by a token
written to `~/Library/Application Support/MacRemote/editors/<pid>.json` (mode 600).

Output is mirrored from the moment the extension starts. With the
`terminalDataWriteEvent` API proposal enabled for this extension in the editor's
`argv.json` (`"enable-proposed-api": ["salimarizi.mac-remote-terminals"]`) it sees
everything a terminal prints; without it, only the output of commands run through
shell integration.

Build: `npm install && npx @vscode/vsce package`, then install the `.vsix` with the
editor's `--install-extension`.
