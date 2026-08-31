# The CLI surface: version, help, and the exit codes.
#
#   vhs scripts/demos/cli.tape

Output docs/assets/demos/cli.gif

Require calendar-bridge

Set Shell "bash"
Set FontSize 15
Set Width 1100
Set Height 620
Set Padding 18
Set Theme "TokyoNight"
Set TypingSpeed 28ms
Set PlaybackSpeed 1.0

Hide
Type "cd /tmp/calendar-bridge-demo && export PATH=/tmp/calendar-bridge-demo:$PATH && clear" Enter
Sleep 500ms
Show

Type "calendar-bridge version" Sleep 400ms Enter
Sleep 2s

Type "clear" Enter
Sleep 300ms

Type "calendar-bridge --help" Sleep 400ms Enter
Sleep 6s

Type "clear" Enter
Sleep 300ms

# Exit codes are documented so a supervisor can branch on them.
Type `calendar-bridge sync-once -config /nope.yaml; echo "exit=$?"` Sleep 400ms Enter
Sleep 2500ms

Type `calendar-bridge bogus >/dev/null 2>&1; echo "exit=$?"` Sleep 400ms Enter
Sleep 2500ms
