#!/bin/sh
# PID 1 stays a shell, so the bench can stop and start mysqld instances freely.
set -e
mkdir -p /data
if [ "$XFS" = "1" ]; then
  mount -o loop /loop/xfs.img /data
fi
chown mysql:mysql /data
if [ ! -d /data/main/mysql ]; then
  mysqld --initialize-insecure --user=mysql --datadir=/data/main
fi
mysqld --user=mysql --datadir=/data/main --socket=/tmp/main.sock --port=3306 --mysqlx=OFF \
  --innodb-buffer-pool-size=1G --pid-file=/tmp/main.pid --log-error=/data/main.err &
exec sleep infinity
