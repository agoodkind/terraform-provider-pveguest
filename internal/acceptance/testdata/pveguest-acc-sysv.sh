#!/bin/sh
### BEGIN INIT INFO
# Provides:          pveguest-acc-sysv
# Required-Start:    $local_fs
# Required-Stop:     $local_fs
# Default-Start:     2 3 4 5
# Default-Stop:      0 1 6
# Short-Description: pveguest acceptance SysV service
### END INIT INFO

case "$1" in
start | stop | restart | force-reload | status)
	exit 0
	;;
*)
	echo "Usage: $0 {start|stop|restart|force-reload|status}" >&2
	exit 3
	;;
esac
