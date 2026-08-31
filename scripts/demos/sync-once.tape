# A sync pass, and what an unauthorized account looks like — which is the
# first thing most people see, because `auth` has to be run per account first.
#
#   vhs scripts/demos/sync-once.tape

Output docs/assets/demos/sync-once.gif

Require calendar-bridge

Set Shell "bash"
Set FontSize 15
Set Width 1100
Set Height 560
Set Padding 18
Set Theme "TokyoNight"
Set TypingSpeed 28ms

Hide
Type "cd /tmp/calendar-bridge-demo && export PATH=/tmp/calendar-bridge-demo:$PATH && clear" Enter
Sleep 500ms
Show

# Before authorizing, the error names the account and the exact fix.
Type "calendar-bridge sync-once -config config.yaml" Sleep 400ms Enter
Sleep 4s

Type "clear" Enter
Sleep 300ms

# -dry-run reports what a pass would change without writing to any calendar.
Type "calendar-bridge sync-once -config config.yaml -dry-run" Sleep 400ms Enter
Sleep 3500ms

Type "clear" Enter
Sleep 300ms

# -json emits one machine-readable report on stdout; logs go to stderr.
Type "calendar-bridge sync-once -config config.yaml -json 2>/dev/null | head -20" Sleep 400ms Enter
Sleep 4s
