git-scan
--------

Run in a git repository to run an oracle as a web server. This oracle currently
only answers if a git commit is inside another one.

Hack
====

    sudo setcap 'cap_net_bind_service=+ep' "$GOPATH/bin/git-scan"
