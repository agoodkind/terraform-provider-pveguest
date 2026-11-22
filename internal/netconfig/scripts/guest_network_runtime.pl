use strict;
use warnings;
use JSON::PP qw(decode_json encode_json);
use Socket qw(AF_INET6 inet_ntop);
use Time::HiRes qw(stat);
my ($operation,$backend) = @ARGV;
my $settings = decode_json(do {local $/; <STDIN>});
my $name = $settings->{name};
my $mac = $settings->{mac};

sub capture {
    my (@argv) = @_;
    open(my $pipe,'-|',@argv) or die "Run $argv[0]: $!\n";
    local $/;
    my $output = <$pipe> // '';
    close($pipe) or die "@argv exited with status ".($? >> 8)."\n";
    return $output;
}
sub check_program {
    my ($program,$package) = @_;
    for my $dir (split /:/,$ENV{PATH}) { return if -x "$dir/$program"; }
    die "Install $package to provide $program for $backend.\n";
}
sub verify_backend {
    my $service = $backend eq 'ifupdown' ? 'networking.service' : 'systemd-networkd.service';
    my $enabled = capture('systemctl','is-enabled',$service);
    die "Enable $service before configuring $name.\n" unless $enabled =~ /^enabled(?:-runtime)?$/m;
    if ($backend eq 'ifupdown') {
        check_program('sysctl','procps');
        check_program('ifup','ifupdown');
        check_program('ifquery','ifupdown');
        check_program('dhclient','isc-dhcp-client') if $settings->{dhcp4};
        check_program('resolvconf','resolvconf') if @{$settings->{dns}} || @{$settings->{search}};
    } else {
        check_program('netplan','netplan.io') if $backend eq 'netplan';
        check_program('networkctl','systemd');
    }
}
sub links_for_mac {
    my $links = decode_json(capture('ip','-j','link','show'));
    return grep { lc($_->{address} // '') eq $mac } @$links;
}
sub link_for_mac {
    my @matches = links_for_mac();
    return (undef,"Expected one interface with MAC $mac; found ".scalar(@matches)) unless @matches == 1;
    return (undef,"MAC $mac belongs to $matches[0]->{ifname}, expected $name") if $matches[0]->{ifname} ne $name;
    return ($matches[0],undef);
}
sub proxy_neighbors {
    my $entries = decode_json(capture('ip','-j','-6','neigh','show','proxy','dev',$name));
    return map { $_->{dst} => 1 } @$entries;
}
sub unmanaged_proxy_neighbors {
    my @managed = (@{$settings->{ipv6_proxy_neighbors} // []}, @{$settings->{obsolete_proxy_neighbors} // []});
    return () unless @managed;
    my %present = proxy_neighbors();
    delete @present{@managed};
    return sort keys %present;
}
sub restore_proxy_neighbors {
    my (@addresses) = @_;
    return unless @addresses;
    my %present = proxy_neighbors();
    for my $address (@addresses) {
        capture('ip','-6','neigh','replace','proxy',$address,'dev',$name) unless $present{$address};
    }
}
sub remove_obsolete_proxy_neighbors {
    my $obsolete = $settings->{obsolete_proxy_neighbors} // [];
    return unless @$obsolete;
    my @links = links_for_mac();
    return unless @links;
    die "Expected one interface named $name with MAC $mac before removing proxy neighbors\n"
        unless @links == 1 && $links[0]->{ifname} eq $name;
    my %present = proxy_neighbors();
    for my $address (@$obsolete) {
        capture('ip','-6','neigh','del','proxy',$address,'dev',$name) if $present{$address};
    }
}
sub check_proxy_neighbors {
    my $wanted = $settings->{ipv6_proxy_neighbors} // [];
    return undef unless @$wanted || @{$settings->{obsolete_proxy_neighbors} // []};
    my %present = proxy_neighbors();
    my @missing = grep { !$present{$_} } @$wanted;
    return "Missing IPv6 proxy neighbors on $name: ".join(', ',@missing) if @missing;
    my @obsolete = grep { $present{$_} } @{$settings->{obsolete_proxy_neighbors} // []};
    return "Obsolete IPv6 proxy neighbors on $name: ".join(', ',@obsolete) if @obsolete;
    if (@$wanted) {
        my $enabled = capture('sysctl','-n',"net/ipv6/conf/$name/proxy_ndp");
        return "IPv6 proxy NDP is disabled on $name" unless $enabled =~ /^1$/m;
    }
    return undef;
}
sub dhcp4_lease {
    my $path = "/var/lib/dhcp/dhclient.$name.leases";
    return unless -f $path;
    my $leases = capture('cat',$path);
    my @blocks = $leases =~ /^lease \{([^}]+)\}/msg;
    return unless @blocks;
    my $last = $blocks[-1];
    my ($address) = $last =~ /^\s*fixed-address ([0-9.]+);/m;
    my ($routers) = $last =~ /^\s*option routers ([0-9., ]+);/m;
    my ($classless) = $last =~ /^\s*option (?:rfc3442-classless-static-routes|classless-static-routes) ([0-9, ]+);/m;
    my %gateways = map { $_=>1 } split /[, ]+/, ($routers // '');
    # RFC 3442 encodes each route as a prefix length, prefix bytes and gateway.
    my @bytes = split /[, ]+/, ($classless // '');
    while (@bytes) {
        my $bits = shift @bytes;
        last if $bits > 32 || @bytes < int(($bits+7)/8)+4;
        splice(@bytes,0,int(($bits+7)/8));
        my $via = join('.',splice(@bytes,0,4));
        $gateways{$via}=1 if $bits==0;
    }
    return ($address,\%gateways);
}
sub loaded_network {
    my ($info)=@_;
    my $base="/etc/systemd/network/05-pveguest-$name";
    return 0 unless ($info->{NetworkFile}//'') eq "$base.network";
    # A matching filename alone cannot prove that networkd read the current
    # settings. Require the managed files to predate this networkd process.
    my $start=capture('busctl','get-property','org.freedesktop.systemd1',
        '/org/freedesktop/systemd1/unit/systemd_2dnetworkd_2eservice',
        'org.freedesktop.systemd1.Service','ExecMainStartTimestamp');
    $start =~ /^t ([1-9][0-9]*)\s*$/ or die "Cannot read the networkd start time: $start";
    my $started=$1/1_000_000;
    for my $path ("$base.network","$base.link") {
        my @metadata=stat($path);
        return 0 unless @metadata && $metadata[10] < $started;
    }
    return 1;
}
sub status {
    my ($link,$error) = link_for_mac();
    return $error if defined $error;
    return "Interface $name is down" unless grep { $_ eq 'UP' } @{$link->{flags}};
    return "MTU on $name differs from $settings->{mtu}" if $settings->{mtu} && $link->{mtu} != $settings->{mtu};
    my $proxy_error = check_proxy_neighbors();
    return $proxy_error if defined $proxy_error;
    my $data = decode_json(capture('ip','-j','address','show','dev',$name));
    my %wanted = map { $_=>1 } @{$settings->{addresses}};
    my ($ipv4,$ipv6) = (0,0);
    my $info;
    if ($backend ne 'ifupdown') { $info=decode_json(capture('networkctl','status','--json=short',$name)); }
    my %ndisc;
    if ($info) {
        for my $entry (@{$info->{Addresses} // []}) {
            next unless ($entry->{ConfigSource}//'') eq 'NDisc' && $entry->{Family}==AF_INET6;
            $ndisc{inet_ntop(AF_INET6,pack('C*',@{$entry->{Address}}))}=1;
        }
    }
    my ($lease_address,$lease_gateways) = dhcp4_lease() if $backend eq 'ifupdown' && $settings->{dhcp4};
    for my $address (@{$data->[0]->{addr_info}}) {
        next if $address->{tentative} || $address->{dadfailed};
        my $cidr = "$address->{local}/$address->{prefixlen}";
        next if delete $wanted{$cidr};
        next unless ($address->{scope} // '') eq 'global';
        $ipv4 = 1 if $address->{family} eq 'inet' && ($backend ne 'ifupdown' || ($lease_address // '') eq $address->{local});
        $ipv6 = 1 if $address->{family} eq 'inet6' && ($info ? $ndisc{$address->{local}} : ($address->{protocol} // '') eq 'kernel_ra');
    }
    return "Missing configured addresses on $name: ".join(', ',sort keys %wanted) if %wanted;
    return "Waiting for a DHCP IPv4 address on $name" if $settings->{dhcp4} && !$ipv4;
    return "Waiting for a SLAAC IPv6 address on $name" if $settings->{slaac} && !$ipv6;
    if ($backend eq 'ifupdown' && $settings->{slaac}) {
        my $value = capture('sysctl','-n',"net/ipv6/conf/$name/accept_ra");
        return "IPv6 RA acceptance on $name must be 2" unless $value =~ /^2$/m;
    }
    for my $family (qw(-4 -6)) {
        my $routes=decode_json(capture('ip','-j',$family,'route','show','default','dev',$name));
        for my $route (@$routes) {
            my $proto = $route->{protocol} // 'boot';
            my $gateway = $route->{gateway} // '';
            next if $family eq '-6' && $proto eq 'ra' && $settings->{slaac};
            next if $family eq '-4' && $settings->{dhcp4} && $proto eq 'dhcp';
            next if $family eq '-4' && $settings->{dhcp4} && $backend eq 'ifupdown' && $proto eq 'boot' && $lease_gateways && $lease_gateways->{$gateway};
            return "Unexpected default route on $name via $gateway ($proto)";
        }
    }
    if ($backend ne 'ifupdown') {
        my $from_dhcp = grep { ($_->{ConfigSource} // '') eq 'DHCPv4' } @{$info->{Addresses} // []};
        return "Waiting for a networkd DHCPv4 lease on $name" if $settings->{dhcp4} && !$from_dhcp;
        return "Restart the guest to load the staged networkd configuration for $name" unless loaded_network($info);
    }
    return undef;
}
if ($operation eq 'remove-proxy-neighbors') {
    remove_obsolete_proxy_neighbors();
    exit;
}
verify_backend();
if ($operation eq 'prerequisites') {exit;}
if ($operation eq 'activate') {
    my ($link,$error) = link_for_mac();
    die "$error\n" if defined $error;
    remove_obsolete_proxy_neighbors();
    if ($backend eq 'ifupdown') {
        my @unmanaged = unmanaged_proxy_neighbors();
        open(my $probe,'-|','ifquery','--state',$name) or die "Run ifquery: $!\n";
        { local $/; <$probe>; }
        close($probe);
        my $code=$? >> 8;
        die "ifquery --state $name failed: $code\n" if $code>1;
        capture('ifdown',$name) if $code==0;
        capture('ifup',$name);
        restore_proxy_neighbors(@unmanaged);
    } else {
        my $info=decode_json(capture('networkctl','status','--json=short',$name));
        die "Restart the guest to load the staged networkd configuration for $name before activation.\n" unless loaded_network($info);
        capture('networkctl','reconfigure',$name);
    }
    for (1..30) {
        my $reason=status();
        if (!defined $reason) {print encode_json({ready=>JSON::PP::true});exit;}
        die "$reason\n" if $_==30;
        sleep 1;
    }
}
die "Unknown guest network operation $operation\n" unless $operation eq 'status';
my $reason=status();
print encode_json({ready=>defined($reason)?JSON::PP::false:JSON::PP::true,reason=>$reason});
