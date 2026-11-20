# bash completion for railcar
_railcar() {
  local cur prev
  cur=${COMP_WORDS[COMP_CWORD]}
  prev=${COMP_WORDS[COMP_CWORD-1]}
  case $prev in
    --profile) COMPREPLY=($(compgen -d -- "$cur")); return ;;
    --log|--config) COMPREPLY=($(compgen -f -- "$cur")); return ;;
    --proxy) return ;;
    --dev-server) return ;;
  esac
  COMPREPLY=($(compgen -W "--profile --key-file --log --proxy --proxy-calls --proxy-from-env --config --dev-server --demo --version --help" -- "$cur"))
}
complete -F _railcar railcar
