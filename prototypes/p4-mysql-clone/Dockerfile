# MySQL 8.4 LTS + mount, so the bench can put the data directory on a loop-mounted XFS.
FROM mysql:8.4.3
RUN microdnf install -y util-linux && microdnf clean all
COPY start.sh /start.sh
ENTRYPOINT ["/start.sh"]
