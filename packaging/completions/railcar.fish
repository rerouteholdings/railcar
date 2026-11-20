# fish completion for railcar
complete -c railcar -l profile -r -a '(__fish_complete_directories)' -d 'Keep the identity, keys, and messages in a directory'
complete -c railcar -l key-file -d 'Keep the profile key in a file when there is no keychain'
complete -c railcar -l log -r -F -d 'Write debug lines to a file'
complete -c railcar -l dev-server -r -d 'Reach a server at a URL, for local test networks'
complete -c railcar -l demo -d 'Run with made-up chats and no network'
complete -c railcar -l version -d 'Print the version'
complete -c railcar -s h -l help -d 'Show the help'
