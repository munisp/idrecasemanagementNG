FROM nginx:1.27-alpine
LABEL org.opencontainers.image.authors="Patrick Munis at newwave"
COPY . /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
