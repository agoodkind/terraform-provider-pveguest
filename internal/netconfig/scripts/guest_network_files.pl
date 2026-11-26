use strict;
use warnings;
use JSON::PP qw(decode_json encode_json);
use File::Basename qw(dirname basename);
use File::Temp qw(tempfile tempdir);
use File::Glob qw(bsd_glob);
use File::Path qw(make_path);
use Fcntl qw(:flock);

my ($backend, $action, $name) = @ARGV;
die "Invalid interface name\n" unless $name =~ /^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,14}$/;
my $main = '/etc/network/interfaces';
my $owner = "/etc/network/pveguest/$name";
my $managed = "/etc/netplan/90-pveguest-$name.yaml";
my $unit_base = "/etc/systemd/network/05-pveguest-$name";
my $bond_dhcp = '/etc/dhcp/pveguest-' . unpack('H*', $name) . '.conf';
my $bond_base = '/etc/network/pveguest-bonds/' . unpack('H*', $name);
my $member_base = '/etc/systemd/network/05-pveguest-member-' . unpack('H*', $name) . '-';
my $netdev = "/etc/systemd/network/10-netplan-$name.netdev";
my $dhclient_hook = '/etc/dhcp/dhclient-enter-hooks.d/pveguest-' . unpack('H*', $name);
my %files;

sub read_file {
    my ($path) = @_;
    open(my $file, '<', $path) or die "Read $path: $!\n";
    local $/;
    my $text = <$file> // '';
    close($file) or die "Close $path: $!\n";
    return $text;
}

sub read_interfaces {
    my ($path) = @_;
    return if exists $files{$path};
    my $text = $files{$path} = read_file($path);
    for my $line (split /\n/, $text) {
        next unless $line =~ /^\s*(source|source-directory)\s+([^#]+)/;
        my ($directive, $targets) = ($1, $2);
        for my $pattern (split ' ', $targets) {
            $pattern = dirname($path) . "/$pattern" unless $pattern =~ m{^/};
            if ($directive eq 'source-directory') {
                for my $directory (bsd_glob($pattern)) {
                    for my $file (bsd_glob("$directory/*")) {
                        read_interfaces($file) if -f $file && basename($file) =~ /^[a-zA-Z0-9_-]+$/;
                    }
                }
            } else {
                for my $file (bsd_glob($pattern)) {
                    next if $file eq $owner && !-e $file;
                    read_interfaces($file) if -f $file;
                }
            }
        }
    }
}

sub scan {
    if ($backend eq 'netplan') {
        my %sources;
        for my $dir (qw(/lib/netplan /etc/netplan /run/netplan)) {
            for my $file (bsd_glob("$dir/*.yaml")) {
                $sources{ basename($file) } = $file if -f $file;
            }
        }
        for my $file (values %sources) { $files{$file} = read_file($file); }
    } elsif ($backend eq 'networkd') {
        for my $file (
            "/etc/systemd/network/$name.network",
            map { "/etc/systemd/network/05-pveguest-$name.$_" } qw(link network),
        ) {
            $files{$file} = read_file($file) if -e $file;
        }
    } elsif ($backend eq 'ifupdown') {
        read_interfaces($main);
        $files{$dhclient_hook} = read_file($dhclient_hook) if -f $dhclient_hook;
        $files{$owner} = read_file($owner) if -e $owner;
        for my $path ("$bond_base.pl", $bond_dhcp, bsd_glob("$member_base*.link")) {
            $files{$path} = read_file($path) if -f $path;
        }
    } else {
        die "Unsupported backend $backend\n";
    }
}

sub planned_unit {
    my ($edits, $path, $content) = @_;
    my $header = "# Managed by pveguest_network_interface\n";
    my $before = -f $path ? read_file($path) : undef;
    return unless defined($before) || defined($content);
    die "Conflicting networkd unit $path\n" if defined($before) && index($before, $header) != 0;
    my $after = defined($content) ? $header . $content : undef;
    return if defined($before) && defined($after) && $before eq $after;
    $edits->{$path} = { before => $before, after => $after };
}

sub bond_units {
    my ($edits, $root, $mac, $members, $remove) = @_;
    my %desired;
    if (!$remove && %$members) {
        my $generated = "$root/run/systemd/network/10-netplan-";
        my $network = read_file("$generated$name.network");
        # Pin both components of the DHCPv4 client identifier across active-member changes.
        my $hex_mac = $mac;
        $hex_mac =~ s/://g;
        my $iaid = hex(substr($hex_mac, -8));
        $network .=
            "\n[DHCPv4]\nClientIdentifier=duid\nIAID=$iaid\nDUIDType=link-layer\nDUIDRawData=00:01:$mac\n";
        $desired{"$unit_base.network"} = $network;
        $desired{$netdev} = read_file("$generated$name.netdev");
        for my $member (keys %$members) {
            my $assigned = $members->{$member};
            for my $suffix (qw(link network)) {
                my $unit = read_file("$generated$member.$suffix");
                $unit =~ s/^(?:Permanent)?MACAddress=\Q$assigned\E$/MACAddress=$assigned/mi
                    or die "Generated $suffix for $member does not match $assigned\n";
                $unit =~ s/^\[Match\]$/[Match]\nDriver=!bonding/m if $suffix eq 'link';
                $desired{ $member_base . unpack('H*', $member) . ".$suffix" } = $unit;
            }
        }
    }
    my %paths = map { $_ => 1 }
        (keys %desired, bsd_glob("$member_base*.link"), bsd_glob("$member_base*.network"));
    $paths{$netdev} = 1
        if -f $netdev
        && (index(read_file($netdev), "# Managed by pveguest_network_interface\n") == 0
            || %$members);
    for my $path (keys %paths) {
        planned_unit($edits, $path, $desired{$path});
    }
    planned_unit($edits, "$unit_base.link", undef) if %$members;
}

sub generated_edits {
    my ($mac, $remove) = @_;
    my $root = tempdir(CLEANUP => 1);
    my %edits;
    if (!$remove) {
        scan();
        my $source_edits = decode_json(do { local $/; <STDIN> });
        for my $path (keys %$source_edits) {
            my $after = $source_edits->{$path}->{after};
            if (defined $after) { $files{$path} = $after; }
            else { delete $files{$path}; }
        }
        for my $path (keys %files) {
            make_path(dirname("$root$path"));
            open(my $out, '>', "$root$path") or die "Create temporary netplan source: $!\n";
            chmod(0600, "$root$path") or die "Set temporary source permissions: $!\n";
            print {$out} $files{$path} or die "Write temporary netplan source: $!\n";
            close($out) or die "Close temporary netplan source: $!\n";
        }
        system('netplan', 'generate', '--root-dir', $root) == 0
            or die "netplan generate in the temporary root failed\n";
    }
    my %members = @ARGV > 4 ? @ARGV[4 .. $#ARGV] : ();
    bond_units(\%edits, $root, $mac, \%members, $remove);
    if (%members) {
        planned_unit(\%edits, "$unit_base.network", undef) if $remove;
        return \%edits;
    }
    for my $suffix (qw(link network)) {
        my $path = "$unit_base.$suffix";
        my $before = -f $path ? read_file($path) : undef;
        my $header = "# Managed by pveguest_network_interface\n";
        die "Conflicting networkd unit $path\n" if defined $before && index($before, $header) != 0;
        if ($remove) { $edits{$path} = { before => $before } if defined $before; next; }
        my @matches;
        for my $file (bsd_glob("$root/run/systemd/network/*.$suffix")) {
            my $content = read_file($file);
            push @matches, $content
                if $content =~ /^(?:Permanent)?MACAddress=\Q$mac\E$/mi
                && $content =~ /^Name=\Q$name\E$/m;
        }
        die "Expected one generated .$suffix file for $name with MAC $mac; found "
            . scalar(@matches) . "\n"
            unless @matches == 1;
        my $selected = $matches[0];
        $selected =~ s/^PermanentMACAddress=\Q$mac\E$/MACAddress=$mac/mi;
        my $after = $header . $selected;
        $edits{$path} = { before => $before, after => $after }
            if !defined($before) || $before ne $after;
    }
    return \%edits;
}

if ($action eq 'render' || $action eq 'unrender') {
    print encode_json(generated_edits($ARGV[3], $action eq 'unrender'));
    exit;
}
if ($action eq 'read') { scan(); print encode_json(\%files); exit; }
die "Unknown operation\n" unless $action eq 'write';
open(my $lock, '>>', '/run/lock/pveguest-network-interfaces.lock')
    or die "Open guest network lock: $!\n";
flock($lock, LOCK_EX) or die "Lock guest network configuration: $!\n";
scan();
if ($backend eq 'netplan') {
    for my $path (bsd_glob("$member_base*"), $netdev) {
        $files{$path} = read_file($path) if -f $path;
    }
    for my $suffix (qw(network link)) {
        my $path = "$unit_base.$suffix";
        $files{$path} = read_file($path) if -f $path;
    }
}
my $edits = decode_json(do { local $/; <STDIN> });
for my $path (keys %$edits) {
    my $bond_file = $backend eq 'ifupdown' && ($path eq "$bond_base.pl" || $path eq $bond_dhcp);
    my $bond_unit = $backend eq 'netplan'
        && ($path eq $netdev || $path =~ /^\Q$member_base\E[0-9a-f]+\.network$/);
    $bond_unit = 1 if $backend ne 'networkd' && $path =~ /^\Q$member_base\E[0-9a-f]+\.link$/;
    die "Unexpected configuration path $path\n"
        unless exists $files{$path}
        || $path eq $managed
        || $path eq $owner
        || $bond_file
        || $bond_unit
        || $backend eq 'ifupdown' && $path eq $dhclient_hook
        || $backend ne 'ifupdown'
        && $path =~ m{^/etc/systemd/network/05-pveguest-\Q$name\E\.(?:network|link)$};
    die "Cannot replace symbolic link $path\n" if -l $path;
    my $before = $edits->{$path}->{before};
    die "$path changed before writing; retry the apply\n"
        if defined($before) != exists($files{$path})
        || defined($before) && $before ne $files{$path};
}
# Write the owned file first. The original settings remain recoverable until
# the new declaration exists. Retry compares every file with a fresh read.
for my $path (
    sort { ($b eq $managed || $b eq $owner) <=> ($a eq $managed || $a eq $owner) || $a cmp $b }
    keys %$edits
) {
    my $after = $edits->{$path}->{after};
    if (!defined $after) { unlink($path) or die "Remove $path: $!\n"; next; }
    my $directory = dirname($path);
    make_path($directory) unless -d $directory;
    my ($file, $tmp) = tempfile('.pveguest-XXXXXXXX', DIR => $directory, UNLINK => 1);
    my @old = stat($path);
    if (@old) {
        chown($old[4], $old[5], $tmp) == 1 or die "Set owner on $tmp: $!\n";
        chmod($old[2] & 07777, $tmp) == 1 or die "Set mode on $tmp: $!\n";
    }
    if (index($path, "/etc/systemd/network/") == 0) {
        chmod(0644, $tmp) == 1 or die "Set networkd unit permissions: $!\n";
    }
    chmod(0755, $tmp) == 1 or die "Set DHCP hook permissions: $!\n" if $path eq $dhclient_hook;
    print {$file} $after or die "Write $tmp: $!\n";
    close($file) or die "Close $tmp: $!\n";
    rename($tmp, $path) or die "Replace $path: $!\n";
}
