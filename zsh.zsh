# Function that calls the adhd client.
function adhd_update() {
  # Call the client in force mode.
  # Adjust the --addr value if your server runs elsewhere.
  adhd_output=$(/home/nils/Documents/projects/time_daemon/adhd -mode=client --force --addr=localhost:8080)
}

# Add the hook to run before each prompt.
autoload -U add-zsh-hook
add-zsh-hook precmd adhd_update
