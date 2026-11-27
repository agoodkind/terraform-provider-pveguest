use strict;
use warnings;
use JSON::PP qw(decode_json);

my ($name, $primary, $miimon, $reselect, %members) = @ARGV;

sub ip {
    system('ip', @_) == 0 or die "ip @_ failed\n";
}

open(my $command, '-|', 'ip', '-j', '-d', 'link', 'show') or die "Inspect Linux links: $!\n";
my $links = decode_json(do { local $/; <$command> });
close($command) or die "Inspect Linux links failed\n";
my %links = map { $_->{ifname} => $_ } @$links;
for my $member (sort keys %members) {
    my $link = $links{$member} or die "Missing bond member $member with MAC $members{$member}\n";
    die "MAC on $member differs from $members{$member}\n"
        unless $link->{address} eq $members{$member};
    die "$member already belongs to $link->{master}\n"
        if defined $link->{master} && $link->{master} ne $name;
}
for my $link (@$links) {
    die "Unexpected member $link->{ifname} on $name\n"
        if ($link->{master} // '') eq $name && !exists $members{ $link->{ifname} };
}
if (my $bond = $links{$name}) {
    die "Restart the guest with the staged member names before configuring $name\n"
        unless ($bond->{linkinfo}->{info_kind} // '') eq 'bond';
    my $options = $bond->{linkinfo}->{info_data};
    die "Restart the guest to load the staged bond options for $name\n"
        unless $options->{mode} eq 'active-backup'
        && $options->{miimon} == $miimon
        && $options->{fail_over_mac} eq 'active'
        && $options->{primary_reselect} eq $reselect;
} else {
    ip(
        'link',
        'add',
        'name',
        $name,
        'type',
        'bond',
        'mode',
        'active-backup',
        'miimon',
        $miimon,
        'fail_over_mac',
        'active',
        'primary_reselect',
        $reselect,
    );
}
for my $member ($primary, grep { $_ ne $primary } sort keys %members) {
    next if ($links{$member}->{master} // '') eq $name;
    ip('link', 'set', 'dev', $member, 'down');
    ip('link', 'set', 'dev', $member, 'master', $name);
    ip('link', 'set', 'dev', $member, 'up');
}
if (($links{$name}->{linkinfo}->{info_data}->{primary} // '') ne $primary) {
    ip('link', 'set', 'dev', $name, 'type', 'bond', 'primary', $primary);
}
