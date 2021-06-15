FROM scratch
COPY plow /usr/bin/plow
ENTRYPOINT ["/usr/bin/plow"]
