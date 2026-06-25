[x] users would like to run apps in their mmicroVMs and have them exposed via url.
[x] machine templates: go development, node dev, python, Go + Python + Node + build essentials... etc.
[x] machine details
[x] bug: bottom bar shows 3 terminals. We have 1 terminal in each machine, 3 machine running. It would be good to have them there, but when you click on the terminal in machine 2, go to machine 2.
[x] update README.md
[x] The bottom of terminal window cuts a bit on the terminal text. We should have a little bit more of room.
[x] When a terminal is maximized, there's no way to un-maximize it.
[x] Errors in claude when running in terminbal:
          ⚠ claude command at /home/dev/.local/bin/claude missing or broken (/home/dev/.local/bin 
            does not exist) · run claude install to repair
          ⚠claude command at /home/dev/.local/bin/claude missing or broken · run claude install to
            repair
[x] The GitHub App only allows repos from my main account. All the repos from the other Orgs I belong, are not there. What do I have to change to be able to work on all those repos?
  https://github.com/apps/tavon-proteos
[x] Let's improve the code server initial configuration. Let's run it with the following flags --disable-workspace-trust --disable-getting-started-override and let's add this file ~/.local/share/code-server/User/settings.json with the following content:
        {
          "git.path": "/usr/bin/git",
          "workbench.colorTheme": "Dark+", 
        }
[x] Maximise window when double click on the top window bar, like in other OS.
[x] Right now, we can only use Claude Code using the cli (or via remote agent). Add pi.dev as a new remote provider.
[ ] Take screenshots and use Claude Design to improve UI
[x] Improve ansible playbook, spit out/copy:
      - cat /etc/proteos/node-agent.env | grep "PROTEOS_ROOTFS_REF="
      - /var/lib/proteos/images/proteos-templates.json
      - tailscale install
[x] Add "Download" button to project: we zip and download the project as it is.
[x] Speed up tests?
ok      github.com/tavon-ai/proteos/controlplane/internal/auth     11.943s
ok      github.com/tavon-ai/proteos/controlplane/internal/github   7.244s
ok      github.com/tavon-ai/proteos/controlplane/internal/guestctl 8.032s
ok      github.com/tavon-ai/proteos/controlplane/internal/httpapi  140.349s
ok      github.com/tavon-ai/proteos/controlplane/internal/injector 7.313s
ok      github.com/tavon-ai/proteos/controlplane/internal/machine  27.772s
ok      github.com/tavon-ai/proteos/controlplane/internal/providers        2.587s
ok      github.com/tavon-ai/proteos/controlplane/internal/session  1.309s
ok      github.com/tavon-ai/proteos/controlplane/internal/store    7.324s
ok      github.com/tavon-ai/proteos/controlplane/internal/token    8.252s
