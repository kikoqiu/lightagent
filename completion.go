package main

// bashCompletion is printed by `lightagent completion bash`.
const bashCompletion = `# lightagent bash completion
_lightagent() {
    local cur cmds opts
    cur="${COMP_WORDS[COMP_CWORD]}"
    cmds="gen-agent-prompt sessions completion help"
    opts="-h --help -v -V --version -c --config -C --dir -r --resume --session -p --prompt --json -q --quiet --log --save --no-save --print-config --model --api-base --stream --markdown --result --web-host --web-port --no-web"
    case "${COMP_WORDS[1]}" in
        sessions)
            COMPREPLY=( $(compgen -W "list show prune" -- "$cur") ); return ;;
        completion)
            COMPREPLY=( $(compgen -W "bash zsh powershell" -- "$cur") ); return ;;
        help)
            COMPREPLY=( $(compgen -W "$cmds" -- "$cur") ); return ;;
        gen-agent-prompt)
            COMPREPLY=( $(compgen -W "-f --force" -- "$cur") ); return ;;
    esac
    if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "$opts" -- "$cur") )
    else
        COMPREPLY=( $(compgen -W "$cmds" -- "$cur") )
    fi
}
complete -F _lightagent lightagent
`

// zshCompletion is printed by `lightagent completion zsh`.
const zshCompletion = `#compdef lightagent
_lightagent() {
  local -a cmds
  cmds=(gen-agent-prompt sessions completion help)
  _arguments \
    '(-h --help)'{-h,--help}'[show help]' \
    '(-v -V --version)'{-v,-V,--version}'[show version]' \
    '(-c --config)'{-c,--config}'[config file]:file:_files' \
    '(-C --dir)'{-C,--dir}'[working directory]:dir:_directories' \
    '(-r --resume)'{-r,--resume}'[resume the saved session]' \
    '--session[session file]:file:_files' \
    '(-p --prompt)'{-p,--prompt}'[one-shot prompt]:text:' \
    '--json[JSON result for -p]' \
    '(-q --quiet)'{-q,--quiet}'[hide tool/info output]' \
    '--log[append rendered output to a file]:file:_files' \
    '--save[always save the session on exit]' \
    '--no-save[never save the session on exit]' \
    '--print-config[print the effective configuration]' \
    '--model[model name]:model:' \
    '--api-base[api base url]:url:' \
    '--stream[streaming]:on or off:(on off)' \
    '--markdown[markdown rendering]:on or off:(on off)' \
    '--result[tool result visibility]:on or off:(on off)' \
    '--web-host[web bind host]:ip:' \
    '--web-port[web port]:port:' \
    '--no-web[disable the web mirror]' \
    '1:command:->cmd'
  case $state in
    cmd) _describe 'command' cmds ;;
  esac
}
_lightagent "$@"
`

// powershellCompletion is printed by `lightagent completion powershell`.
const powershellCompletion = `# lightagent PowerShell completion
Register-ArgumentCompleter -Native -CommandName lightagent -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $commands = 'gen-agent-prompt','sessions','completion','help'
    $options = '-h','--help','-v','-V','--version','-c','--config','-C','--dir',
               '-r','--resume','--session','-p','--prompt','--json','-q','--quiet',
               '--log','--save','--no-save','--print-config','--model','--api-base',
               '--stream','--markdown','--result','--web-host','--web-port','--no-web'
    ($commands + $options) |
        Where-Object { $_ -like "$wordToComplete*" } |
        ForEach-Object {
            [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
        }
}
`
