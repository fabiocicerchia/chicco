# Regenerate the dashboard GIF with charmbracelet/vhs:
#   go build -o "$(go env GOPATH)/bin/chicco" ./cmd/chicco   # put chicco on PATH
#   vhs docs/demo.tape                                        # -> docs/demo.gif
# Uses the offline demo config (docs/demo.chicco.yaml) — no keys, no network.
Output docs/demo.gif

Set FontSize 15
Set Width 1100
Set Height 620
Set Padding 18

Type "chicco -config docs/demo.chicco.yaml"
Enter
Sleep 4s
Type "t"          # test every configured model
Sleep 6s
Type "q"          # quit
Sleep 1s
