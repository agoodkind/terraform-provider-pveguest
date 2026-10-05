# terraform-provider-pveguest

`pveguest` is an OpenTofu provider that declares files, downloads, symbolic links, apt
packages, deb packages, systemd units, and sysrepo modules and data inside Proxmox guests. The provider calls the
Proxmox VE API with an API token. A container runs each command through its
`exec` API, and a VM runs each command through the QEMU guest agent API. The
provider does not require SSH access to the hypervisor or the guest.

The provider address is `tofu.home.arpa/agoodkind/pveguest`. The provider
supports Debian guests.

## Prerequisites

- Go 1.27.1 or later, for the build.
- OpenTofu 1.11 or later. `content_wo` requires write-only attribute support.
- A Proxmox VE API token for each hypervisor, with the privileges in
  [API privileges](#api-privileges).
- For `kind = "lxc"`: a Proxmox VE node that provides the container `exec`,
  `exec-status`, `file-write`, and `file-read` API methods. Stock Proxmox VE does
  not provide them.
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
    suburban = {
      endpoint  = "https://suburban.example.net:8006"
      api_token = var.suburban_api_token
    }
  }
}

variable "suburban_api_token" {
  type      = string
  sensitive = true
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
| `nodes` | map of object | yes | | Hypervisors by name. A resource selects one with its `node` argument. |
| `nodes.<name>.node_name` | string | no | the map key | Proxmox node name in the API path. Set it when the Proxmox node name differs from the map key. |
| `nodes.<name>.endpoint` | string | yes | | URL of the Proxmox VE API, for example `https://10.230.0.254:8006`. |
| `nodes.<name>.api_token` | string, sensitive | yes | | API token in the form `user@realm!tokenid=secret`. |
| `nodes.<name>.insecure` | bool | no | `false` | Skips TLS certificate verification. |
| `max_requests` | number | no | `4` | Concurrent API requests per hypervisor. |
| `controller_ca_file` | string | no | | Path of a PEM file with certificate authorities that the controller trusts in addition to the system roots when `pveguest_download` uses `fetch = "controller"`. |

## API privileges

The token needs these privileges on `/vms/<vmid>` of each guest that a resource
manages. A token with privilege separation needs the privileges in its own
permissions, not only in those of its user.

| Guest kind | API methods | Privileges |
| --- | --- | --- |
| `lxc` | `exec`, `exec-status` | `VM.Guest.Exec` |
| `qemu` | `agent/exec`, `agent/exec-status` | `VM.GuestAgent.Unrestricted` |

The provider runs every guest operation through `exec` and `exec-status`. The
file-write and file-read methods are not used.

## Command execution

An exec call starts the command and returns an identifier. The provider polls
`exec-status` until the command exits. The delay between polls starts at 200 ms
and doubles up to 2 s. A command has a timeout of 120 seconds by default. The
wait ends 15 seconds after that timeout, or when OpenTofu cancels the
operation.

A result with a truncated output stream or a timed-out command fails with an
error that states the guest and the command. Every API error states the guest
(node, vmid, kind), the API path, the HTTP status, and the response text. A
stopped container, a stopped VM, and a VM without a running guest agent produce
an error that starts with the guest and contains "is unreachable".

The container API accepts at most 96 KiB of standard input per command. The
QEMU guest agent API accepts text of at most 64 KiB per request, and the
provider limits each command to 16 KiB.

## Guest arguments

Every resource has these arguments. A change to one of them replaces the
resource.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `node` | string | yes | Key in the provider `nodes` map. The API path uses the `node_name` of that entry, or the key when `node_name` is unset. |
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

Content larger than the standard input limit of one command takes several
commands. One command truncates the temporary file, and one command per piece
appends to it. A piece has at most 96 KiB for a container and 16 KiB for a VM.
The validate, mode, owner, and rename steps run after the last piece.

## pveguest_download

A regular file that comes from an HTTPS URL, optionally from one member of a
`.tar.gz` archive, and that must have a given SHA-256 hash. Read runs `stat` and
`sha256sum`. Read removes a missing file from state, and the next plan creates
it. A guest file with a hash that differs from `file_sha256` produces a planned
rewrite. A changed `url`, `sha256`, `archive_member`, or `fetch` produces a
planned rewrite. A plan does not download anything. Destroy deletes the file.

An apply runs `mkdir -p` for the directory of `path`. Both fetch modes write a
temporary file in that directory, compare its hash, set mode and owner, and
rename it to `path`. A failed step deletes every temporary file. The file at
`path` has its earlier content.

With `fetch = "guest"`, the guest runs `curl -fsSL --proto =https`. A hash of
the downloaded object that differs from `sha256` fails the apply with an error
that states the guest, the URL, the expected hash, and the received hash. With
`archive_member`, `curl` writes the archive to a temporary file and the guest
runs `tar -xzf <archive> -O -- <member>` into the second temporary file. The
guest steps have the exec timeout of 600 seconds each.

With `fetch = "controller"`, the machine that runs OpenTofu downloads `url`
into the cache directory `$XDG_CACHE_HOME/pveguest`, or `~/.cache/pveguest`.
The cache file name is the SHA-256 hash. A cached file with a matching hash
skips the download, and a download with another hash fails the apply. With
`archive_member`, the controller extracts that member with Go `archive/tar`
and `compress/gzip`, and rejects a member path that is absolute or contains
`..`. The controller compresses the payload with gzip and writes the compressed
stream to a temporary file in the guest in chunks of at most 96 KiB. The guest
runs `gzip -dc` into the second temporary file. The provider compares the
SHA-256 hash of that file with the hash that the controller computed from the
payload. `fetch = "controller"` requires `kind = "lxc"`, because the QEMU guest agent
API accepts only text.

| Argument | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `url` | string | yes | | HTTPS URL. The guest needs network access to it with `fetch = "guest"`. |
| `sha256` | string | yes | | Expected hash of the object at `url` as 64 lowercase hexadecimal characters. With `archive_member`, the hash of the archive. |
| `fetch` | string | no | `guest` | `guest` runs `curl` in the guest. `controller` downloads on the machine that runs OpenTofu and sends the file through the exec API. |
| `archive_member` | string | no | | Relative path of one file inside a `.tar.gz` archive at `url`. The installed file is that member. |
| `path` | string | yes | | Absolute path. An apply creates each missing parent directory with mode `0755` and owner `root:root`. A change replaces the resource. |
| `mode` | string | no | `0644` | Four octal digits. |
| `owner` | string | no | `root` | User name. |
| `group` | string | no | `root` | Group name. |

| Attribute | Meaning |
| --- | --- |
| `file_sha256` | SHA-256 hash of the installed file in the guest. |
| `write_id` | Random identifier that changes each time an apply writes the file, mode, owner, or group. |

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

## pveguest_deb_packages

Packages that are installed from `.deb` files in the guest, for example files
that `pveguest_download` writes. Read runs `dpkg-query --show` for each package
and `dpkg-deb -f <path> Version` for each file. Read removes a package from
state when it is not installed or its installed version differs from the
version of its file. A missing file does not remove the package.

An apply installs every missing or different package in one call:
`apt-get install -y --no-install-recommends --no-download -- <paths>` with
`DEBIAN_FRONTEND=noninteractive`. The provider runs one apt operation per
guest at a time. A dependency that the guest lacks fails the apply with the
error text of apt. Destroy removes the resource from state and leaves the
packages installed.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `packages` | map of string | yes | Map from Debian package name to the absolute guest path of its `.deb` file. |

| Attribute | Meaning |
| --- | --- |
| `deb_versions` | Map from package name to the installed version. |

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

## pveguest_sysrepo_module

A YANG module in the sysrepo repository of the guest. The guest needs
`sysrepoctl` from sysrepo 3. The module file must already exist in the guest,
for example from `pveguest_file`. Read runs `sysrepoctl --list`. Read removes
the resource from state when the module is not installed. Read stores the
installed revision and the enabled features, and a difference from the
declaration produces a planned update. The provider runs one sysrepo operation
per guest at a time.

The file name is `<module>@<revision>.yang`, and the module name and revision
come from it. A changed module name replaces the resource. Destroy runs
`sysrepoctl --uninstall <module>`.

Create and Update run these `sysrepoctl` commands in order:

1. When the module is not installed: `sysrepoctl --install <path> --search-dirs
   <directory of path> --enable-feature <feature> ...`. A module that another
   module only imports counts as not installed.
2. When the module is installed at another revision and `update` is `true`:
   `sysrepoctl --update <path> --search-dirs <directory of path>`. With
   `update = false` the apply fails and states both revisions.
3. When the enabled features differ from `features`: `sysrepoctl --change
   <module> --enable-feature <feature> ... --disable-feature <feature> ...`.

Each apply then runs `sysrepoctl --list` and fails when the revision or the
features differ from the declaration.

| Argument | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `path` | string | yes | | Absolute path of the module file in the guest. The directory of the file resolves imports. |
| `features` | set of string | no | empty | Features to enable. The apply disables every other enabled feature. |
| `update` | bool | no | `false` | Whether an apply replaces a module that is installed at another revision. |

| Attribute | Meaning |
| --- | --- |
| `module` | Module name from the file name. |
| `revision` | Revision from the file name in the plan, and the installed revision after a refresh. |

## pveguest_sysrepo_data

A subtree of configuration data in the `startup` or `running` datastore of the
guest. The guest needs `sysrepocfg` from sysrepo 3, and the module that defines
the data must be installed. Read removes the resource from state when the
module is not installed.

Create and Update validate `content` as XML and write it to a temporary file in
the guest. Then they run `sysrepocfg --edit=<temporary file> --datastore
<datastore> --module <module> --format xml`, which merges the content into the
datastore, and delete the temporary file. A merge adds and changes nodes and
removes none. A node in the datastore that `content` omits stays after the
apply and remains a difference. `content` must define the whole subtree.

Read runs `sysrepocfg --export --datastore <datastore> --xpath <xpath> --format
xml --defaults explicit`. When the canonical form of the export differs from
the canonical form of the content in state, Read stores the export as the
content, and the plan shows an update. `content` must have the form that the
export prints for `xpath`, including the ancestors of a selected node.

The canonical form is the Go `encoding/xml` token stream with these changes:

- Comments, processing instructions, and directives are dropped.
- Namespace declarations and prefixes are dropped. Each element and attribute
  keeps its namespace URI and local name.
- Attributes are sorted by namespace URI and local name.
- Whitespace at the start and end of text is trimmed, and text that has only
  whitespace is dropped. Adjacent text and CDATA sections merge.
- Element order, list entry order, and text inside the elements stay as they
  are.

Destroy builds a copy of the content in state with the NETCONF operation
`remove` on each top-level element and merges it with the same `sysrepocfg
--edit` command. The command removes the whole subtree of each top-level
element, including nodes that `content` omits. A destroy with empty content in
state removes nothing.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `datastore` | string | yes | `startup` or `running`. A change replaces the resource. |
| `module` | string | yes | Name of the YANG module that defines the content. A change replaces the resource. |
| `xpath` | string | yes | XPath of the subtree that `content` defines. A change replaces the resource. |
| `content` | string | yes | XML document with the data of the subtree. |

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
  PVEGUEST_ACC_ENDPOINT=https://suburban.example.net:8006 \
  PVEGUEST_ACC_TOKEN_FILE=$HOME/.config/pveguest/token \
  PVEGUEST_ACC_VMID=224 \
  PVEGUEST_ACC_KIND=lxc
```

| Variable | Meaning |
| --- | --- |
| `PVEGUEST_ACC_NODE` | Proxmox node name and key in the provider `nodes` map. |
| `PVEGUEST_ACC_ENDPOINT` | URL of the Proxmox VE API. |
| `PVEGUEST_ACC_TOKEN_FILE` | Path of a file that contains the full API token string. |
| `PVEGUEST_ACC_VMID` | ID of the test guest. |
| `PVEGUEST_ACC_KIND` | `lxc` or `qemu`. |
| `PVEGUEST_ACC_INSECURE` | `true` skips TLS certificate verification. The default is `false`. |

`make testacc` sets `TF_ACC=1`, sets `TF_ACC_TERRAFORM_PATH` to the `tofu`
binary, and sets `TF_ACC_PROVIDER_HOST` and `TF_ACC_PROVIDER_NAMESPACE` to the
provider address.

The tests change only these items in the guest, and each test removes its
items at the end:

- Files and links under `/root/pveguest-acc/`.
- The package `hello`. The cleanup purges it.
- The units `pveguest-acc.timer` and `pveguest-acc.service` under
  `/etc/systemd/system/`.
- The sysrepo module `pveguest-acc` and its data in the `running` datastore.
  `TestAccSysrepoModuleAndData` skips when the guest has no `sysrepoctl` or
  `sysrepocfg`.

`TestAccDownload` and `TestAccDownloadHashMismatch` download
`https://www.rfc-editor.org/rfc/rfc1149.txt`. The test guest needs network
access to `www.rfc-editor.org` over HTTPS.

`TestAccDownloadControllerTransfer`, `TestAccDownloadArchiveMember`, and
`TestAccDebPackages` serve their objects from a local HTTPS server in the test
process. The test writes the server certificate to a file and sets
`controller_ca_file` in the provider block to that file. Each test sets
`XDG_CACHE_HOME` to a temporary directory. `TestAccDownloadControllerTransfer`
sends 30 MiB of random data and logs the elapsed time of the apply with
`t.Logf`. `TestAccDebPackages` builds a package with `dpkg-deb` and skips when
the controller lacks `dpkg-deb`. It installs and purges the package
`pveguest-acc-test`.

The last test, `TestAccGuestIsClean`, fails when one of these items is still
in the guest. `TESTARGS` passes extra arguments to `go test`, for example
`TESTARGS="-run TestAccLink"`.

One test runs the `tofu` binary against the installed provider in a temporary
directory and reads the raw state file and the saved plan. The other tests use
`terraform-plugin-testing` with the provider in the test process.
