use strict;
use warnings;
use JSON::PP qw(decode_json encode_json);
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
sub link_for_mac {
    my $links = decode_json(capture('ip','-j','link','show'));
    my @matches = grep { lc($_->{address} // '') eq $mac } @$links;
    return (undef,"Expected one interface with MAC $mac; found ".scalar(@matches)) unless @matches == 1;
    return (undef,"MAC $mac belongs to $matches[0]->{ifname}, expected $name") if $matches[0]->{ifname} ne $name;
    return ($matches[0],undef);
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
sub status {
    my ($link,$error) = link_for_mac();
    return $error if defined $error;
    return "Interface $name is down" unless grep { $_ eq 'UP' } @{$link->{flags}};
    return "MTU on $name differs from $settings->{mtu}" if $settings->{mtu} && $link->{mtu} != $settings->{mtu};
    my $data = decode_json(capture('ip','-j','address','show','dev',$name));
    my %wanted = map { $_=>1 } @{$settings->{addresses}};
    my ($ipv4,$ipv6) = (0,0);
    my ($lease_address,$lease_gateways) = dhcp4_lease() if $backend eq 'ifupdown' && $settings->{dhcp4};
    for my $address (@{$data->[0]->{addr_info}}) {
        next if $address->{tentative} || $address->{dadfailed};
        my $cidr = "$address->{local}/$address->{prefixlen}";
        next if delete $wanted{$cidr};
        next unless ($address->{scope} // '') eq 'global';
        $ipv4 = 1 if $address->{family} eq 'inet' && ($backend ne 'ifupdown' || ($lease_address // '') eq $address->{local});
        $ipv6 = 1 if $address->{family} eq 'inet6' && ($address->{dynamic} || ($address->{valid_life_time} // 4294967295) < 4294967295);
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
        my $info = decode_json(capture('networkctl','status','--json=short',$name));
        my $from_dhcp = grep { ($_->{ConfigSource} // '') eq 'DHCPv4' } @{$info->{Addresses} // []};
        return "Waiting for a networkd DHCPv4 lease on $name" if $settings->{dhcp4} && !$from_dhcp;
        my $file = $info->{NetworkFile} // '';
        return "networkd has not selected the managed configuration for $name ($file)" unless $file =~ m{/05-pveguest-\Q$name\E\.network$};
    }
    return undef;
}
verify_backend();
if ($operation eq 'prerequisites') {exit;}
if ($operation eq 'activate') {
    my ($link,$error) = link_for_mac();
    die "$error\n" if defined $error;
    if ($backend eq 'ifupdown') {
        open(my $probe,'-|','ifquery','--state',$name) or die "Run ifquery: $!\n";
        { local $/; <$probe>; }
        close($probe);
        my $code=$? >> 8;
        die "ifquery --state $name failed: $code\n" if $code>1;
        capture('ifdown',$name) if $code==0;
        capture('ifup',$name);
    } else {
        capture('networkctl','reload');
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
