# IDRE edge image.
#
# Default build is stock Caddy (config mounted at runtime):
#
#   docker build -t idre-edge .
#
# The open-appsec WAF variant is a separate build target — open-appsec ships
# as a native Caddy module, which is precisely why Caddy (not nginx, not
# APISIX) is the WAF attachment point for north-south traffic:
#
#   docker build --target appsec -t idre-edge:appsec .
#
# The appsec image embeds the open-appsec agent; attach it to your deployment
# at https://my.openappsec.io (SaaS management) or mount a declarative local
# policy under /etc/caddy/appsec before exposing ports 80/443.

FROM caddy:2.8-builder AS appsec-build
RUN xcaddy build \
	--with github.com/openappsec/attachment

FROM caddy:2.8-alpine AS appsec
COPY --from=appsec-build /usr/bin/caddy /usr/bin/caddy

# Default target: stock Caddy. Must stay the LAST stage in this file.
FROM caddy:2.8-alpine AS stock
