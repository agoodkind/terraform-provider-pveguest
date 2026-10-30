# terraform-provider-pveguest

`pveguest` is an OpenTofu provider that declares files, downloads, symbolic links, apt
packages, deb packages, systemd units, and sysrepo modules and data inside Proxmox guests. The provider calls the
Proxmox VE API with an API token. The provider uses the guest command API under `/nodes/{node}/lxc/{vmid}/` for containers or `/nodes/{node}/qemu/{vmid}/` for virtual machines. The node uses the QEMU guest agent to run virtual machine commands. The
provider does not require SSH access to the hypervisor or the guest.

The provider address is `tofu.home.arpa/agoodkind/pveguest`. The provider
supports Debian guests.

## Prerequisites

- Go 1.27.1 or later, for the build.
- OpenTofu 1.11 or later. `content_wo` requires write-only attribute support.
- A Proxmox VE API token for each hypervisor, with the privileges in
  [API privileges](#api-privileges).
- The Proxmox VE node must provide `exec`, `exec-status`, and `exec-ack` for
  each guest kind and support acknowledged result retention. Stock Proxmox VE
  does not provide these methods.
- A guest with `kind = "qemu"` requires a running QEMU guest agent.

## Install

```sh
make install-mirror
```

`make install-mirror` builds the provider for the host platform with version
`0.1.1` and writes the binary to
`~/.terraform.d/plugins/tofu.home.arpa/agoodkind/pveguest/0.1.1/<os>_<arch>/terraform-provider-pveguest_v0.1.1`.
OpenTofu reads that directory as an implied local mirror.

A configuration selects the provider by its address:

```hcl
terraform {
  required_providers {
    pveguest = {
      source  = "tofu.home.arpa/agoodkind/pveguest"
      version = "0.1.1"
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
| `nodes.<name>.max_exec_requests` | number | no | `4` | This setting limits concurrent guest commands for this node configuration and requires a value of at least 1. |
| `max_requests` | number | no | `4` | Concurrent API requests per hypervisor. |
| `controller_ca_file` | string | no | | Path of a PEM file with certificate authorities that the controller trusts in addition to the system roots when `pveguest_download` uses `fetch = "controller"`. |
| `smtp2go_api_key` | string, sensitive | no | | The key authenticates SMTP2GO requests for `pveguest_mail_relay`. The provider uses `PVEGUEST_SMTP2GO_API_KEY` when the argument is empty. |
| `smtp2go_api_url` | string | no | `https://api.smtp2go.com/v3` | The URL selects the SMTP2GO endpoint. The provider accepts HTTPS, or HTTP for localhost and loopback addresses. |

## API privileges

The token needs these privileges on `/vms/<vmid>` of each guest that a resource
manages. A token with privilege separation needs the privileges in its own
permissions, not only in those of its user.

| Guest kind | API methods | Privileges |
| --- | --- | --- |
| `lxc` | `exec`, `exec-status`, `exec-ack` | `VM.Guest.Exec` |
| `qemu` | `exec`, `exec-status`, `exec-ack` | `VM.Guest.Exec` |

The token needs these privileges on `/nodes/<node>` for
`pveguest_host_kernel_modules`.

| API method | Privilege |
| --- | --- |
| `GET /nodes/{node}/kernel-modules` | `Sys.KernelModules.Audit` |
| `PUT /nodes/{node}/kernel-modules` | `Sys.KernelModules.Modify` |

The token needs these privileges on `/vms/<vmid>` for
`pveguest_container_power`.

| API method | Privilege |
| --- | --- |
| `POST /nodes/{node}/lxc/{vmid}/status/start` | `VM.PowerMgmt` on `/vms/<vmid>` |
| `POST /nodes/{node}/lxc/{vmid}/status/shutdown` | `VM.PowerMgmt` on `/vms/<vmid>` |
| `POST /nodes/{node}/lxc/{vmid}/status/stop` | `VM.PowerMgmt` on `/vms/<vmid>` |

## Command execution

Each `POST exec` request includes `"result-retention": "acknowledged"`.
Older overlays reject this parameter before starting a command.

An exec call starts the command and returns an identifier. The provider polls
`exec-status` until the command exits. The delay between polls starts at 200 ms
and doubles up to 2 s. A command has a timeout of 120 seconds by default. The
wait ends 15 seconds after that timeout, or when OpenTofu cancels the
operation.

`GET exec-status` returns the completed result repeatedly until acknowledgment
or expiry after one hour. After a lost result reply, the provider reads the
same identifier again. The provider validates the complete response and decodes
both output streams before sending `POST exec-ack` with a numeric `pid`.
An acknowledgment failure logs a cleanup warning without repeating the command.
The provider returns the decoded result or the command timeout or capture-limit error.

Each command reserves an exec slot before its POST exec request and releases the slot after the command exits or fails. The slot covers that request and every exec-status poll. `max_exec_requests` limits concurrent commands across all resources that use the same node configuration in one provider process. A caller waits for a slot until it can reserve one or its context ends. `max_requests` limits individual API requests in flight and releases each request slot between exec-status polls.

A result with a truncated output stream or a timed-out command fails with an
error that states the guest and the command. Every API error states the guest
(node, vmid, kind), the API path, the HTTP status, and the response text. A
stopped container, a stopped VM, and a VM without a running guest agent produce
an error that starts with the guest and contains "is unreachable".

The exec API accepts at most 96 KiB (98304 bytes) of standard input per command for either guest kind because `input-data` has a limit of 131072 base64 characters.

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

If an API connection closes, each file inspection command can run at most
three times. The provider stops retrying when OpenTofu cancels the operation.

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
| `write_id` | The provider assigns a new random `write_id` whenever an apply writes the content, mode, owner, or group. After import, Read sets `write_id` to `import-` followed by the first 16 hexadecimal characters of the guest `sha256`. |

With `content_wo`, Read compares the hash of the file in the guest with the
hash stored at the last apply. A difference produces a planned rewrite. The
provider writes a changed `content_wo` value when `content_wo_version` also
changes.

Content larger than the standard input limit of one command takes several
commands. One command truncates the temporary file, and one command per piece
appends to it. Each piece has at most 96 KiB for either guest kind.
The validate, mode, owner, and rename steps run after the last piece.

Import IDs use the format `<node>/<vmid>/<kind>/<absolute path>`, such as
`suburban/224/lxc//etc/example.conf`. Import reads the content hash, mode,
owner, and group without writing the file. Read sets `write_id` to `import-`
followed by the first 16 hexadecimal characters of the guest `sha256`. Matching
`content`, `mode`, `owner`, and `group` keep that `write_id`. A difference plans
a write that assigns a new random `write_id`.

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
The cache file name is the SHA-256 hash. The provider process serializes
requests for the same hash. Each request checks the cache after acquiring
the hash lock. A matching cached file skips the download, and a download
with another hash fails the apply. With
`archive_member`, the controller extracts that member with Go `archive/tar`
and `compress/gzip`, and rejects a member path that is absolute or contains
`..`. The controller compresses the payload with gzip and writes the compressed
stream to a temporary file in the guest in chunks of at most 96 KiB. The guest
runs `gzip -dc` into the second temporary file. The provider compares the
SHA-256 hash of that file with the hash that the controller computed from the
payload.

Each header name must be an HTTP token containing only letters, digits, and
the characters ``!#$%&'*+-.^_`|~``. A header value must not contain a carriage
return, a line feed, or a NUL character. Validation errors identify the header
without printing its value. Apply replaces each header value with `[redacted]`
in every error message.

With `fetch = "controller"`, the provider process on the machine that runs
OpenTofu sends each header on its HTTPS request. The HTTP client follows at
most 10 redirects. A redirect to a different scheme, host, or port removes
every configured header from the redirected request. A signed asset URL on
another host does not receive the headers.

With `fetch = "guest"`, the provider writes each header as a
`header = "Name: value"` line to the standard input of `curl --config -`.
The headers do not appear in command arguments or the process list. With
headers set, `curl` runs without `-L` and does not follow redirects. A redirect
response fails the apply with an error that directs the operator to set
`fetch = "controller"`.

| Argument | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `url` | string | yes | | HTTPS URL. The guest needs network access to it with `fetch = "guest"`. |
| `sha256` | string | yes | | Expected hash of the object at `url` as 64 lowercase hexadecimal characters. With `archive_member`, the hash of the archive. |
| `fetch` | string | no | `guest` | `guest` runs `curl` in the guest. `controller` downloads on the machine that runs OpenTofu and sends the file through the exec API. |
| `archive_member` | string | no | | Relative path of one file inside a `.tar.gz` archive at `url`. The installed file is that member. |
| `request_headers_wo` | map of string, sensitive, write-only | with `request_headers_wo_version` | | Each key is an HTTP request header name and each value is its header value, for example `Authorization = "Bearer <token>"`. OpenTofu does not store the header values or the plan value in state or plan files. A change to this argument alone does not plan a change. |
| `request_headers_wo_version` | number | with `request_headers_wo` | | A changed value plans a rewrite. Apply downloads the object again with the headers from the current configuration unless a cached object with the expected hash skips the request. |
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
apt-get waits up to 300 seconds for the package lock during installation.

Apply installs the exact package versions in `versions`, including downgrades. Read records the installed versions for those packages. Apply runs `apt-mark hold` for each package in `locked` and runs `apt-mark unhold` for packages removed from `locked`. Read reports which packages in `locked` still appear in `apt-mark showhold`.

When package removal is enabled, the provider runs `apt-mark unhold` before `apt-get remove` for installed packages selected for removal. When remove_on_delete is true, the plan for pveguest_apt_packages shows a warning that lists the declared packages that the provider removes during apply or destroy.

`start_services` defaults to `true`. When `start_services` is `false`, the provider creates `/usr/sbin/policy-rc.d` only while its `apt-get` runs `dpkg` and prevents package scripts from starting services. Other package tools wait for the `dpkg` locks, and pveguest installs use a shared lock. The provider refuses to proceed if an existing policy file lacks its marker, and it deletes a marked stale file after the `dpkg` locks are free. A later `pveguest_systemd_unit` resource can start the service.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `packages` | set of string | yes | Specify Debian package names without version or architecture suffixes. |
| `versions` | map of string | no | Map package names to exact Debian versions. Every key must also appear in `packages`. |
| `locked` | set of string | no | Select packages to lock with `apt-mark hold`. Every name must also appear in `packages`. |
| `remove_on_delete` | bool | no | Set to `true` to remove packages dropped from `packages` during apply and all listed packages during destroy. When `false` or unset, these removal operations change only OpenTofu state. |
| `start_services` | bool | no | Defaults to `true`, which permits packages to start services normally. Set to `false` to prevent package scripts from starting or restarting services during installation. |

## pveguest_deb_packages

Packages that are installed from `.deb` files in the guest, for example files
that `pveguest_download` writes. Read runs `dpkg-query --show` for each package
and `dpkg-deb -f <path> Version` for each file. Read removes a package from
state when it is not installed or its installed version differs from the
version of its file. A missing file does not remove the package.

An apply installs every missing or different package from its declared file.
apt-get permits downgrades and waits up to 300 seconds for the package lock.
The install excludes recommended packages and rejects dependencies that
require a download. The provider serializes apt operations per guest.
Destroy removes the resource from state and retains the installed packages.

`start_services` uses the same behavior as `pveguest_apt_packages`. Both resources share `/run/pveguest-package-install.lock`.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `packages` | map of string | yes | Map from Debian package name to the absolute guest path of its `.deb` file. |
| `start_services` | bool | no | Defaults to `true`, which permits packages to start services normally. Set to `false` to prevent package scripts from starting or restarting services during installation. |

| Attribute | Meaning |
| --- | --- |
| `deb_versions` | Map from package name to the installed version. |

## pveguest_systemd_unit

The enabled state and the active state of one unit. Read runs
`systemctl is-enabled` and `systemctl is-active`. Read removes a unit from
state when its unit file does not exist.

Create and Update run `systemctl daemon-reload` and reconcile the declared
unit state. When `restart_on` changes and the unit remains enabled, the
provider runs `systemctl reenable` to rebuild its installation links before
reconciling its active state. Create fails when the unit file does not exist.
When `remove_on_delete` is `true`, Destroy stops an active unit, disables an enabled unit, and reads its state again; a missing unit counts as removed. Destroy fails and keeps the resource in OpenTofu state if a guest command fails or the unit remains active or enabled; when `remove_on_delete` is `false` or unset, Destroy removes only the resource from state without running a guest command.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `name` | string | yes | Full unit name with suffix, for example `ssh.service`. A change replaces the resource. |
| `enabled` | bool | yes | Whether the unit is enabled. A unit in the state `static`, `alias`, `indirect`, `generated`, or `transient` satisfies both values. |
| `active` | bool | yes | Whether the unit is active. |
| `restart_on` | map of string | no | A changed map restarts an active unit during apply. |
| `remove_on_delete` | bool | no | When `true`, Destroy stops the unit if active and disables it if enabled. Defaults to `false`, which removes only the resource from OpenTofu state. |

The intended `restart_on` value is the `write_id` of each file that the unit
reads. The `sha256` of a file is the same before a hand edit and after the
rewrite that repairs it. The `write_id` changes at each write, and the changed
value triggers the restart.

Create requests a restart when `restart_on` has at least one entry. It also
rebuilds installation links when the unit is already enabled and the
declaration requires it to remain enabled.

Import IDs use the format `<node>/<vmid>/<kind>/<name>`, such as
`suburban/224/lxc/ssh.service`, with both `tofu import` and an `import` block.
The provider rejects other ID shapes and shows the required format; it checks
`kind` and `name` against the argument rules. Import fails if the unit file does
not exist in the guest. Import sets `remove_on_delete` to `false`, and Read sets
`enabled` and `active` from the guest. The first apply after import enables,
disables, starts, or stops the unit as the configuration requires. During that
apply, the provider skips the restart and `systemctl reenable` only if the prior
`restart_on` is null and every planned `restart_on` value is `import-` followed
by exactly 16 lowercase hexadecimal characters; any other value, including a
literal string, restarts the unit. Every later `restart_on` change restarts the
unit.

## pveguest_locales

The resource declares a set of generated locales in a guest. Read runs `locale -a` and stores the declared entries that exist. An absent declared locale produces a planned update.

Read compares each entry's first field with `locale -a` output after normalizing the codeset. The normalized names include `en_US.utf8` for `en_US.UTF-8` and `de_DE.iso885915@euro` for `de_DE.ISO-8859-15@euro`. A name without a codeset remains unchanged.

The guest requires the `locales` package and the declared entries in `/etc/locale.gen`. Create and Update run `locale-gen` with a 600-second timeout when a declared locale is absent. Update also runs `locale-gen` when `generate_on` changes. The apply fails if a declared locale remains absent after generation. Destroy removes the resource from state without deleting generated locales.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `locales` | set of string | yes | Each entry uses the form `<locale> <charmap>`, for example `en_US.UTF-8 UTF-8`. The guest's `/etc/locale.gen` must contain each declared entry. |
| `generate_on` | map of string | no | The map accepts arbitrary string values, usually the `write_id` of the `/etc/locale.gen` file. A changed map runs `locale-gen` during Update even when every declared locale exists. Create does not use the map. |

## pveguest_mail_relay

The `pveguest_mail_relay` resource manages one SMTP2GO user and one guest configuration file at `/etc/msmtprc` by default. The provider generates a 32-character password from letters and digits with `crypto/rand`. State stores `password_version`, the file hash, and the non-secret arguments without storing the password. The resource requires `smtp2go_api_key` or `PVEGUEST_SMTP2GO_API_KEY`.

The provider runs `mkdir -p` in the guest before `POST /users/smtp/add`. An existing user requires `POST /users/smtp/edit`. The provider writes a temporary file, sets mode `0600` and owner `root:root`, and renames the file to the destination. A failure after the SMTP2GO request plans another password update and file write at the next apply with a new password.

Read runs `sha256sum` and `POST /users/smtp/view`. A missing file removes the resource from state. A different file hash plans a rewrite with a new password. A missing SMTP2GO user produces a warning and removes the resource from state. A failed view request or a status other than `allowed` produces a warning without itself planning a change.

Destroy deletes the guest file without deleting the SMTP2GO user. A change to `description` alone sends no request. A decrease in `password_version` fails the plan.

| Argument | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `username` | string | yes | | The username selects the SMTP2GO user that msmtp authenticates as. A changed value replaces the resource. |
| `description` | string | no | | The description applies when SMTP2GO creates the user. |
| `host` | string | yes | | The host sets the mail server hostname or address in the guest configuration. |
| `port` | number | yes | | The port sets the mail server port from 1 through 65535. |
| `from` | string | yes | | The address sets the sender email address in the guest configuration. |
| `path` | string | no | `/etc/msmtprc` | The path sets the absolute destination of the guest configuration file. A changed value replaces the resource. |
| `password_version` | number | yes | | An increase in `password_version` generates a new password and rewrites the guest file. |

| Attribute | Meaning |
| --- | --- |
| `sha256` | The attribute records the SHA-256 hash of the guest file content in hexadecimal. |
| `write_id` | The attribute records a new random identifier after each successful file write. |

### Import an existing relay

The SMTP2GO user and guest file must already exist. The file must contain an `account smtp2go` section with one value each for `host`, `port`, `user`, and `from`. The `user` value must match the import username.

1. Set `host`, `port`, `from`, and `password_version` in the resource configuration to match the existing relay.
2. Import the relay with an ID in the form `<node>/<vmid>/<kind>/<username>/<password_version>`:

   ```shell
   tofu import pveguest_mail_relay.relay suburban/224/lxc/host-clyde/1
   ```

   To use a different file path, append a clean absolute path to the ID. For example, `suburban/224/lxc/host-clyde/1/etc/msmtprc` uses `/etc/msmtprc`.

3. Run `tofu plan` and review the result before applying it.

Import fails if the ID is invalid, the file is missing, or the required file fields are missing or invalid. OpenTofu reports that the import object does not exist if SMTP2GO does not list the user.

Import and the following apply do not send an SMTP2GO password request or write the file when the configuration matches the imported values and the file hash matches the applied hash. If the configuration sets `description`, the first plan shows an in-place update to `description` only. That apply does not send an SMTP2GO request or write the file.

## pveguest_sysrepo_module

This resource manages a YANG module and its enabled features in a guest.
The guest requires sysrepo 3 and `sysrepoctl`. The module file must already
exist at `path`, for example through `pveguest_file`.

| Operation | Behavior |
| --- | --- |
| Create | The provider installs an absent module with `sysrepoctl --install`. An imported but unimplemented module also requires installation. |
| Update | The provider runs `sysrepoctl --update` when the installed revision differs and `update = true`. With `update = false`, the provider preserves an installed module. |
| Feature changes | Installation enables the declared features. With `update = true`, the provider also reconciles features of installed modules through `sysrepoctl --change`. |
| Read | The provider removes an uninstalled module from state. With `update = true`, refresh also records the installed revision and features. |
| Destroy | The provider runs `sysrepoctl --uninstall`. |

The apply verifies the installed revision and features after mutation.
One provider instance serializes sysrepo mutations per guest. Reads and
separate provider instances do not share that serialization.

| Argument | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `path` | string | yes | | The absolute guest path must end in `<module>@<YYYY-MM-DD>.yang`. The file directory is the import search directory. |
| `features` | set of string | no | empty | Installation enables this set. With `update = true`, apply also reconciles the enabled features of installed modules. |
| `update` | bool | no | `false` | A true value reconciles the revision and features of an installed module. A false value preserves it. |

| Attribute | Meaning |
| --- | --- |
| `module` | The provider parses the module name from the file name. A changed name replaces the resource. |
| `revision` | The plan uses the file name revision. With `update = true`, refresh records the installed revision. |

## pveguest_sysrepo_data

This resource replaces the complete configuration of one installed YANG
module in the guest's `startup` or `running` datastore. The guest requires
sysrepo 3 and `sysrepocfg`. Apply removes nodes omitted from `content`.

Create and Update require XML with at least one element. The provider writes
a temporary guest file and imports it with `sysrepocfg --import`, selecting
the datastore, module, and XML format. The provider attempts to delete the
temporary file after the write or import finishes. Destroy imports an empty file
to remove the module configuration.

Read exports the module with `sysrepocfg --export --defaults explicit`.
Read removes an uninstalled module from state. A different canonical export
replaces `content` in state, and the next plan restores the declaration.
Matching canonical content retains the declared XML formatting.

Canonical comparison preserves element order, namespace URIs, local names,
and text after trimming its leading and trailing whitespace. It combines
adjacent text and CDATA sections, sorts attributes by namespace URI and local
name, and ignores whitespace-only text, comments, processing instructions,
directives, namespace prefixes, and namespace declarations. Content must use
a representation compatible with the export.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `datastore` | string | yes | Select `startup` or `running`. A change replaces the resource. |
| `module` | string | yes | The installed YANG module must define the content. A change replaces the resource. |
| `content` | string | yes | Supply the complete module configuration as XML. |

## pveguest_host_kernel_modules

This resource replaces the node's complete kernel module list. Apply loads
the modules and configures loading at boot. Delete empties the list. The node
never unloads modules. The node must provide the overlay methods `GET` and
`PUT /nodes/{node}/kernel-modules`. Stock Proxmox VE does not provide these
methods. The token needs `Sys.KernelModules.Audit` and
`Sys.KernelModules.Modify` on `/nodes/<node>`. Import uses the node key.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `node` | string | yes | Select a key in the provider's `nodes` map. A change replaces the resource. |
| `modules` | set of string | yes | Supply the complete module set. An empty set clears the list. Each name must match `^[a-z0-9_]+$`. The node rejects the entire request if any module is absent from its allowlist, unknown to `modinfo`, or built into the kernel. Read stores only loaded entries in `modules`. A configured module that is listed but not loaded produces a planned update. The next apply removes unconfigured entries that are not loaded. |

| Attribute | Meaning |
| --- | --- |
| `loaded` | The computed map reports each listed module as true when `/sys/module/<name>` exists and false otherwise. |

## pveguest_host_kernel_modules data source

The data source reads the node's persistent kernel module list and load states
with `GET /nodes/{node}/kernel-modules`. It requires
`Sys.KernelModules.Audit` on `/nodes/<node>`. The data source does not change
the list or load modules. A module can appear in `modules` while its `loaded`
value is false.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `node` | string | yes | Select a key in the provider's `nodes` map. |

| Attribute | Type | Meaning |
| --- | --- | --- |
| `modules` | set of string | The set contains every module in the persistent list. |
| `loaded` | map of bool | Each value reports whether the listed module is loaded. |

## pveguest_container_options

The resource configures eBPF token delegation with `bpfdelegate` and passes host network interfaces into the container as LXC physical devices with `hostnic0` through `hostnic9`.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `node` | string | Yes | The node must be a key in the provider `nodes` map. Changing `node` replaces the resource. |
| `vmid` | number | Yes | The value identifies the container. Changing `vmid` replaces the resource. |
| `options` | map of string | Yes | Keys must be `bpfdelegate` or `hostnic0` through `hostnic9`. Values are Proxmox property strings, such as `link=nic2,name=wan`. |

| Attribute | Meaning |
| --- | --- |
| `pending` | The computed map contains supported options that Proxmox lists as pending for a running container. Values contain the pending value. Pending deletions have an empty value. |

Create and update write the declared options and delete keys present in prior state but absent from the configuration. Destroy deletes every option key in state.

Read stores every supported option from the container configuration in `options`. Changes and deletions on the node plan an update. Keys added on the node that the configuration omits also plan an update.

The resource does not start, stop, or restart the container. Proxmox applies changes to a running container at the next start.

Import IDs use the format `<node>/<vmid>`.

## pveguest_vm_options

The resource uses the same arguments, computed attribute, apply, Read, Destroy, and import behavior as [pveguest_container_options](#pveguest_container_options), for a VM instead of a container.

The only supported key is `vsock`. A value of `1` adds a virtio vsock device (`vhost-vsock-pci`) with the VM ID as its guest CID. A value of `0` disables it. The node must run the proxmox-overlays qemu-server patch because stock Proxmox VE does not have `vsock`.

The resource uses these API paths and privileges:

| Operation | API path | Privilege |
|---|---|---|
| Read config | `GET /nodes/{node}/qemu/{vmid}/config` | `VM.Audit` on `/vms/<vmid>` |
| Read pending | `GET /nodes/{node}/qemu/{vmid}/pending` | `VM.Audit` on `/vms/<vmid>` |
| Write config | `PUT /nodes/{node}/qemu/{vmid}/config` | `VM.Config.Vsock` on `/vms/<vmid>` |

The resource does not start, stop, or restart the VM. For a running VM, Proxmox lists a `vsock` change as pending and applies it at the next VM start. For a stopped VM, Proxmox writes the value to the config at once.

## pveguest_container_power

`pveguest_container_power` sets the run state of one existing Proxmox LXC container through the Proxmox API. The resource does not create, delete, or configure the container.

The provider waits for the `POST status/start` task to stop and requires exit status `OK`. Shutdown uses `POST status/shutdown` with a 60-second timeout. The provider calls `POST status/stop` if shutdown fails or the container still runs. A failed task produces an error containing the last 10 lines of the Proxmox task log.

Refresh reads `status/current` and sets `running` from the returned status. OpenTofu plans a start when a container stops outside OpenTofu and the configuration has `running = true`. Destroying the resource removes it from state without changing the container run state.

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `node` | string | Yes | The node argument selects the Proxmox node by a key in the provider nodes map. Changing node replaces the resource. |
| `vmid` | number | Yes | The vmid argument specifies the numeric ID of the container. Changing vmid replaces the resource. |
| `running` | bool | Yes | A value of true starts a stopped container. A value of false shuts down a running container. |
| `restart_on` | map of string | No | The restart_on map accepts arbitrary string values, such as the write_id of a file resource. The provider shuts down the container and starts it again when running is true and any map value changes. |

| Attribute | Meaning |
| --- | --- |
| `status` | The status attribute reports the Proxmox status/current string from the last apply or refresh. Values include running and stopped. |

Import IDs use the format `<node>/<vmid>`.

## pveguest-docker-socket

`pveguest-docker-socket` serves the Docker Engine API on a local Unix socket
with mode `0600`. Each supported request runs one
`curl --unix-socket /var/run/docker.sock` command in a Proxmox container or
VM. The existing transport package starts the command with `POST exec` and
polls `GET exec-status`. The command returns the Docker daemon's HTTP
status, response headers, and body.

### Start the command

The guest must have `curl` and a Docker daemon that listens on
`/var/run/docker.sock`. The token file must contain
`user@realm!tokenid=secret`. The token needs these privileges:

| Privilege | Scope |
|---|---|
| `VM.Audit` | `/vms/<vmid>` |
| `VM.Guest.Exec` | `/vms/<vmid>` |

1. Start the command before running `tofu plan`:

   ```sh
   go run ./cmd/pveguest-docker-socket \
     -node suburban \
     -endpoint https://pve.example.com:8006/ \
     -node-name suburban \
     -token-file /path/to/token \
     -vmid 217 \
     -socket /tmp/pveguest-docker.sock
   ```

2. Configure the `kreuzwerker/docker` provider to use the local socket:

   ```hcl
   provider "docker" {
     host = "unix:///tmp/pveguest-docker.sock"
   }
   ```

### Flags

| Flag | Requirement or default | Behavior |
|---|---|---|
| `-node` | Required | The command uses this provider node key for the guest. |
| `-endpoint` | Required | The transport uses this Proxmox API base URL. |
| `-node-name` | Optional | The transport uses this Proxmox node name in API paths. |
| `-token-file` | Required | The command reads the API token from this file. |
| `-vmid` | Required | The command selects this guest ID. |
| `-kind` | `lxc` | The command selects an `lxc` container or a `qemu` VM. |
| `-insecure` | `false` | The transport skips TLS certificate verification when this flag is true. |
| `-socket` | Required | The command listens on this local Unix socket. |
| `-get-only` | `false` | The command allows only GET and HEAD when this flag is true. |
| `-max-body-bytes` | `268435456` (256 MiB) | The command rejects larger request bodies with HTTP 501 before starting any guest exec. |

### Rejected and failed requests

The command returns HTTP 501 with a reason before starting a guest exec
when any of these conditions apply:

- The request sets an `Upgrade` header or includes `upgrade` in the `Connection` header.
- The `follow` query value is `1` or `true`, with any letter case for `true`.
- The last path segment is `events`.
- Any path segment is `attach`.
- The last path segment is `stats`, and the `stream` query value is neither `0` nor `false`, with any letter case for `false`.
- The request body exceeds `-max-body-bytes`.
- The command uses `-get-only`, and the request method is neither GET nor HEAD.

The command returns HTTP 502 when any of these conditions apply:

- A guest exec fails.
- curl exits with a nonzero code.
- A staging step fails.
- Proxmox truncates an output stream.
- The guest response body is incomplete.

For mkdir, dd, or curl failures, the response includes the program name and
the exit code or error.

### Request limits

| Limit | Value |
|---|---|
| Request body per exec | 96 KiB on standard input |
| Largest request body | `-max-body-bytes`, which defaults to 256 MiB |
| Proxmox exec output | 1 MiB per output stream |
| Guest command timeout | 60 seconds; staged curl uses 60 seconds plus 2 seconds per complete MiB of body, up to 3600 seconds |
| Command execution and polling deadline | Each exec timeout plus 30 seconds |

### Stage larger request bodies

The command sends a body of at most 96 KiB directly to curl on standard
input with `--data-binary @-`. For a larger body, the command performs
these steps:

1. It runs `mkdir -m 0700 -- <directory>` to create a directory with the path
   `/tmp/pveguest-docker-socket-<16 random hex characters>`.
2. It runs
   `dd of=<directory>/body oflag=append conv=notrunc status=none`
   once per body piece, in order. Each exec supplies at most 96 KiB on
   standard input. dd appends each piece without a shell redirect.
3. It runs
   `curl --unix-socket /var/run/docker.sock --data-binary @<directory>/body`
   for the request.
4. It attempts `rm -r -f -- <directory>` after every outcome, including
   a failure.

A failed removal produces an error log entry and does not change the response.

The guest must provide these programs:

- `curl`
- `mkdir`
- `dd`
- `rm`

These execs do not use a shell. Staging uses the existing token privileges.
The command does not call the file-write method.

## Development

| Command | Action |
| --- | --- |
| `make build` | Runs vet, every lint gate, and `govulncheck`, then builds `dist/terraform-provider-pveguest`. |
| `make test` | Runs `go test ./...`. The acceptance tests skip unless `TF_ACC` is set. |
| `make check` | Runs every lint gate. |
| `make fmt` | Applies the configured Go formatters. |
| `make install-mirror` | Builds the provider with version `0.1.1` and writes it to the implied local mirror. |
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
| `PVEGUEST_ACC_KERNEL_MODULE` | Module from the node allowlist to use in the acceptance test. The test skips when unset. |
| `PVEGUEST_ACC_HOSTNIC_LINK` | Host interface to use in the acceptance test. The test skips when unset. |
| `PVEGUEST_ACC_VM_VMID` | ID of a stopped test VM on `PVEGUEST_ACC_NODE`. The token needs `VM.Audit`, `VM.Config.Vsock`, and `VM.PowerMgmt` on `/vms/<vmid>`. The test sets and deletes `vsock`, starts and stops the VM, and restores the earlier `vsock` value at the end. The test fails if `TF_ACC` is set and this variable is unset. |
| `PVEGUEST_ACC_UNLISTED_KERNEL_MODULE` | The acceptance test uses a module that exists for the node kernel and is absent from the node allowlist. The test skips when unset. |
| `PVEGUEST_ACC_AUDIT_TOKEN_FILE` | Path of a file containing an API token with `Sys.KernelModules.Audit` and without `Sys.KernelModules.Modify`. The test skips when unset. |
| `PVEGUEST_ACC_SMTP2GO_API_KEY_FILE` | File containing the SMTP2GO API key for `TestAccMailRelayImport`. The test fails if `TF_ACC` is set and this variable is missing. |

`make testacc` sets `TF_ACC=1`, sets `TF_ACC_TERRAFORM_PATH` to the `tofu`
binary, and sets `TF_ACC_PROVIDER_HOST` and `TF_ACC_PROVIDER_NAMESPACE` to the
provider address.

The tests change only these items in the guest, and each test removes its
items at the end:

- Files and links under `/root/pveguest-acc/`.
- The tests install `hello` and `sl`. Cleanup removes the hold on `hello` and purges both packages.
- The tests install packages `at`, `pveguest-acc-svc`, and `pveguest-acc-svc-two`, create units `pveguest-acc-svc.service` and `pveguest-acc-svc-two.service`, and create generated `.deb` files, `postinst-sleep`, and `.times` files under `/root/pveguest-acc/`. Test cleanup purges the packages and deletes the directory.
- The tests use `/usr/sbin/policy-rc.d`, the staged copy `/usr/sbin/policy-rc.d.pveguest-staged.<pid>`, the apt configuration `/run/pveguest-apt-policy.<pid>.conf`, and `/run/pveguest-package-install.lock`. The provider deletes the staged copy and apt configuration when the script ends. Test cleanup deletes the lock file.
- The units `pveguest-acc.timer` and `pveguest-acc.service` under
  `/etc/systemd/system/`.
- The unit `pveguest-acc-removal.service` under `/etc/systemd/system/`.
- The sysrepo module `pveguest-acc` and its data in the `running` datastore.
  `TestAccSysrepoModuleAndData` skips when the guest has no `sysrepoctl` or
  `sysrepocfg`.

`TestAccMailRelayImport` also changes an SMTP2GO account:

- The test creates a user with `pveguest-acc-` followed by eight random
  hexadecimal characters as its username.
- Test cleanup deletes the user through `POST /users/smtp/remove`.
- The controller needs HTTPS access to `api.smtp2go.com`.
- The controller needs TCP access to `mail.smtp2go.com` on port 587.

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
