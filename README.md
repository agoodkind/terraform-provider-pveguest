# terraform-provider-pveguest

`pveguest` is an OpenTofu provider that declares files, symbolic links, apt
packages, and systemd units inside Proxmox guests. The provider opens SSH to
the hypervisor and runs each command with `pct exec` for a container or
`qm guest exec` for a VM. The provider does not require SSH access to the
guest.

The provider address is `tofu.home.arpa/agoodkind/pveguest`. The provider
supports Debian guests.

## Prerequisites

- Go 1.27.1 or later, for the build.
- OpenTofu 1.11 or later. `content_wo` requires write-only attribute support.
- An SSH agent at `SSH_AUTH_SOCK` with a key that the hypervisor accepts for
  the configured user.
- An entry for each hypervisor host in `~/.ssh/known_hosts`. The provider
  connects only to a host that `known_hosts` lists.
- For `kind = "qemu"`: a running QEMU guest agent in the VM.

## Install

```sh
make install-mirror
```

`make install-mirror` builds the provider for the host platform with version
`0.1.0` and writes the binary to
`~/.terraform.d/plugins/tofu.home.arpa/agoodkind/pveguest/0.1.0/<os>_<arch>/terraform-provider-pveguest_v0.1.0`.
OpenTofu reads that directory as an implied local mirror.

A configuration selects the provider by its address:

```hcl
terraform {
  required_providers {
    pveguest = {
      source  = "tofu.home.arpa/agoodkind/pveguest"
      version = "0.1.0"
    }
  }
}

provider "pveguest" {
  nodes = {
    suburban = { host = "suburban.example.net" }
  }
}

resource "pveguest_file" "sshd_base" {
  node     = "suburban"
  vmid     = 224
  kind     = "lxc"
  path     = "/etc/ssh/sshd_config.d/10-base.conf"
  content  = "PasswordAuthentication no\n"
  validate = "sshd -t -f %s"
}

resource "pveguest_systemd_unit" "ssh" {
  node    = "suburban"
  vmid    = 224
  kind    = "lxc"
  name    = "ssh.service"
  enabled = true
  active  = true
  restart_on = {
    base = pveguest_file.sshd_base.write_id
  }
}
```

## Provider arguments

| Argument | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `nodes` | map of object | yes | | Hypervisors by node name. |
| `nodes.<name>.host` | string | yes | | SSH host name or address. |
| `nodes.<name>.port` | number | no | `22` | SSH port. |
| `nodes.<name>.user` | string | no | `root` | SSH user. |
| `max_sessions` | number | no | `4` | Concurrent SSH sessions per hypervisor. |

The provider keeps one SSH connection per hypervisor and runs each command in
its own session.

## Guest arguments

Every resource has these arguments. A change to one of them replaces the
resource.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `node` | string | yes | Key in the provider `nodes` map. |
| `vmid` | number | yes | Proxmox ID of the guest. |
| `kind` | string | yes | `lxc` or `qemu`. |

Read returns an error that includes `node`, `vmid`, and `kind` for a stopped
container, a stopped VM, and a VM with a stopped guest agent.

## pveguest_file

A regular file. Read runs `stat` and `sha256sum`. Read removes a missing file
from state, and the next plan creates it. Destroy deletes the file.

An apply runs `mkdir -p` for the directory of `path`, writes a temporary file
in that directory, sets mode and owner, runs `validate`, and renames the
temporary file to `path`. A failed `validate` command fails the apply before
the rename and deletes the temporary file. The file at `path` has its earlier
content.

| Argument | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `path` | string | yes | | Absolute path. An apply creates each missing parent directory with mode `0755` and owner `root:root`. Destroy deletes the file. A change replaces the resource. |
| `content` | string | one of `content`, `content_wo` | | File content. |
| `content_wo` | string, write-only | one of `content`, `content_wo` | | Specify file content that OpenTofu does not store in plans or state. Also set `content_wo_version`. |
| `content_wo_version` | number | with `content_wo` | | A changed value writes `content_wo` again. |
| `mode` | string | no | `0644` | Four octal digits. |
| `owner` | string | no | `root` | User name. |
| `group` | string | no | `root` | Group name. |
| `validate` | string | no | | Shell command that runs on the temporary file before the rename. The shell-quoted path of the temporary file replaces each `%s`. |

| Attribute | Meaning |
| --- | --- |
| `sha256` | SHA-256 hash of the content in the guest. With `content`, the plan shows the hash before apply. |
| `write_id` | Random identifier that changes each time an apply writes the content, mode, owner, or group. |

With `content_wo`, Read compares the hash of the file in the guest with the
hash stored at the last apply. A difference produces a planned rewrite. The
provider writes a changed `content_wo` value when `content_wo_version` also
changes.

A VM file larger than 1 MiB exceeds the stdin limit of `qm guest exec`, and
the provider returns an error before it runs the command.

## pveguest_link

A symbolic link. Read runs `readlink`. Read removes a missing link, and a path
that is not a link, from state. Destroy deletes the link.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `path` | string | yes | Absolute path of the link. A change replaces the resource. |
| `target` | string | yes | Specify the symbolic link's target path. The target does not need to exist. |

## pveguest_apt_packages

A set of installed apt packages. Read runs `dpkg-query --show` and stores the
declared packages that are installed. The next plan adds each missing package.

An apply with at least one missing package runs `apt-get update` and then
`apt-get install -y --no-install-recommends` with
`DEBIAN_FRONTEND=noninteractive`. A failed `apt-get update` fails the apply
with its own error. The provider runs one apt operation per guest at a time.

A package remains installed after its name is removed from `packages` and
after destroy.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `packages` | set of string | yes | Specify Debian package names without version or architecture suffixes. |

## pveguest_systemd_unit

The enabled state and the active state of one unit. Read runs
`systemctl is-enabled` and `systemctl is-active`. Read removes a unit from
state when its unit file does not exist.

Create and Update run `systemctl daemon-reload` and then `enable`, `disable`,
`start`, `stop`, or `restart` as the declaration requires. Create fails when
the unit file does not exist. Destroy removes the resource from state.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `name` | string | yes | Full unit name with suffix, for example `ssh.service`. A change replaces the resource. |
| `enabled` | bool | yes | Whether the unit is enabled. A unit in the state `static`, `alias`, `indirect`, `generated`, or `transient` satisfies both values. |
| `active` | bool | yes | Whether the unit is active. |
| `restart_on` | map of string | no | A changed map restarts an active unit during apply. |

The intended `restart_on` value is the `write_id` of each file that the unit
reads. The `sha256` of a file is the same before a hand edit and after the
rewrite that repairs it. The `write_id` changes at each write, and the changed
value triggers the restart.

Create restarts a unit that is already active when `restart_on` has at least
one entry.

## Development

| Command | Action |
| --- | --- |
| `make build` | Runs vet, every lint gate, and `govulncheck`, then builds `dist/terraform-provider-pveguest`. |
| `make test` | Runs `go test ./...`. The acceptance tests skip unless `TF_ACC` is set. |
| `make check` | Runs every lint gate. |
| `make fmt` | Applies the configured Go formatters. |
| `make install-mirror` | Builds the provider with version `0.1.0` and writes it to the implied local mirror. |
| `make testacc` | Runs `make install-mirror` and then the acceptance tests. |
| `make help` | Lists every target. |

## Acceptance tests

The acceptance tests change a real guest. Use a dedicated test guest.

```sh
make testacc \
  PVEGUEST_ACC_NODE=suburban \
  PVEGUEST_ACC_HOST=suburban.example.net \
  PVEGUEST_ACC_VMID=224 \
  PVEGUEST_ACC_KIND=lxc
```

| Variable | Meaning |
| --- | --- |
| `PVEGUEST_ACC_NODE` | Node name for the provider `nodes` map. |
| `PVEGUEST_ACC_HOST` | SSH host of the hypervisor. |
| `PVEGUEST_ACC_VMID` | ID of the test guest. |
| `PVEGUEST_ACC_KIND` | `lxc` or `qemu`. |

`make testacc` sets `TF_ACC=1`, sets `TF_ACC_TERRAFORM_PATH` to the `tofu`
binary, and sets `TF_ACC_PROVIDER_HOST` and `TF_ACC_PROVIDER_NAMESPACE` to the
provider address.

The tests change only these items in the guest, and each test removes its
items at the end:

- Files and links under `/root/pveguest-acc/`.
- The package `hello`. The cleanup purges it.
- The units `pveguest-acc.timer` and `pveguest-acc.service` under
  `/etc/systemd/system/`.

The last test, `TestAccGuestIsClean`, fails when one of these items is still
in the guest. `TESTARGS` passes extra arguments to `go test`, for example
`TESTARGS="-run TestAccLink"`.

One test runs the `tofu` binary against the installed provider in a temporary
directory and reads the raw state file and the saved plan. The other tests use
`terraform-plugin-testing` with the provider in the test process.
