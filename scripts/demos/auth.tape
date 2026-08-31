# The authorization flow, up to the point where it needs a browser.
#
# The client ID in the URL belongs to a fabricated fixture project, not to a
# real Google Cloud project.
#
#   vhs scripts/demos/auth.tape

Output docs/assets/demos/auth.gif

Require calendar-bridge

Set Shell "bash"
Set FontSize 15
Set Width 1100
Set Height 560
Set Padding 18
Set Theme "TokyoNight"
Set TypingSpeed 30ms

Hide
Type "cd /tmp/calendar-bridge-demo && export PATH=/tmp/calendar-bridge-demo:$PATH && clear" Enter
Sleep 500ms
Show

Type "calendar-bridge auth -config config.yaml -account personal" Sleep 500ms Enter
Sleep 3s

# You would open that URL, approve, and paste the redirect URL back here.
# The flow forces the consent prompt so Google always returns a refresh token,
# and verifies the state parameter on the URL you paste.
Sleep 4s
Ctrl+C
Sleep 1500ms
