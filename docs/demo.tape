# Regenerate the dashboard GIF with charmbracelet/vhs:
#   vhs docs/demo.tape   ->   docs/demo.gif
# Requires a chicco.yaml with a couple of providers in the working directory.
Output docs/demo.gif

Set FontSize 16
Set Width 1000
Set Height 520
Set Padding 16

Type "chicco -config chicco.yaml"
Enter
Sleep 3s
Type "t"          # test every configured model
Sleep 5s
Type "q"          # quit
Sleep 1s
