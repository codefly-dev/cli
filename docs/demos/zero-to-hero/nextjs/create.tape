Output web.mp4

Set FontSize 20
Set Width 1000
Set Height 1000
Set TypingSpeed 100ms


Type "# Creating a new application to host everything web related"
Enter
Sleep 1s

Type "codefly add application awesome-frontend"
Sleep 1s
Enter

Sleep 3s

Type "# Adding a new service to your application with nexjs+auth0 plugin"
Enter
Sleep 1s



Type "codefly add service web --plugin=codefly.ai/nextjs-auth0:0.0.1"
Enter

Sleep 5s

Type"# Looking at your current workspace"
Sleep 1s
Enter
Sleep 3s

Type "codefly context current"
Sleep 1s
Enter
Sleep 4s

Type "# Create a partial to run the backend and frontend so we can actually develop!"
Enter
Sleep 1s

Type "codefly add partial myworkspace awesome-frontend awesome-backends"
Enter
Sleep 1s


Type "# Let's run it!"
Enter
Sleep 1s


Type "codefly run partial myworkspace"
Sleep 1s
Enter

Sleep 200s