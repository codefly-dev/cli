Output go.mp4

Set FontSize 20
Set Width 1000
Set Height 1000
Set TypingSpeed 100ms


Type "# Creating a new project"
Sleep 1s
Enter
Sleep 1s

Type "codefly add project super-demo"
Sleep 1s
Enter

Sleep 3s

Type "# Creating a new application in your newly minted project"
Enter
Sleep 1s

Type "codefly add application awesome-backends"
Sleep 1s
Enter

Sleep 3s

Type "# Adding a new service to your application with go-grpc agent"
Sleep 1s
Enter
Sleep 1s


Type "codefly add service mybackend --agent=codefly.ai/go-grpc:0.0.1"
Sleep 1s
Enter

Sleep 5s
Enter

Sleep 3s
Enter

Sleep 3s
Enter

Sleep 10s

Type"# Looking at your current workspace"
Sleep 1s
Enter
Sleep 3s

Type "codefly context current"
Sleep 1s
Enter
Sleep 4s

Type "# Let's run it!"
Sleep 1s
Enter
Sleep 1s

Type "codefly run application awesome-backends"
Sleep 1s
Enter
Sleep 8s
