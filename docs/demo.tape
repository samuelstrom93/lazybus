# README demo: browse a dead-letter queue, edit a property, resubmit.
# Regenerate from the repo root (needs vhs):
#
#   vhs docs/demo.tape
#
# It builds ./lazybus (gitignored) and records `lazybus --demo` into
# docs/demo.gif. Keep the GIF small (under ~2 MB).

Output docs/demo.gif

Require go

Set Shell "bash"
Set FontSize 14
Set Width 1120
Set Height 580
Set Padding 10
Set Framerate 10
Set TypingSpeed 90ms
Set Theme "Catppuccin Mocha"

Hide
Type "go build -o lazybus ./cmd/lazybus && clear" Enter
Sleep 15s
Show

Type "./lazybus --demo" Sleep 400ms Enter
Sleep 2s

# Entities: down to orders, open its dead-letter queue.
Type "2" Sleep 600ms
Type "jjj" Sleep 600ms
Enter
Sleep 2s

# Properties tab of the first message.
Type "]"
Sleep 2s

# Edit orderId in the main pane.
Type "0" Sleep 500ms
Type "j" Sleep 700ms
Type "e"
Sleep 1s
Ctrl+U
Type "4102"
Sleep 800ms
Enter
Sleep 2s

# Resubmit: the confirm popup shows the diff; y confirms.
Type "r"
Sleep 3.5s
Type "y"
Sleep 3s

# The cursor is on the next message.
Type "["
Sleep 2.5s
Type "q"
Sleep 500ms
