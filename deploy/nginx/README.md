# Nginx 配置

1. DNS：`ntfy.DOMAIN` 和 `bark.DOMAIN` 两条 A 记录指向服务器。
2. 应用：`tg-route.conf` 放到 `/etc/nginx/snippets/`，在现有站点的 443 server 块里加一行 `include /etc/nginx/snippets/tg-route.conf;`，应用就挂在 `https://DOMAIN/tg/`。
3. ntfy 和 Bark：`tg-channel.conf` 里的 `DOMAIN` 换成你的域名，放到 `/etc/nginx/sites-enabled/`。
4. 证书：`certbot certonly --nginx -d ntfy.DOMAIN -d bark.DOMAIN`。
5. `nginx -t && systemctl reload nginx`。
