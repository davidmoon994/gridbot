# VPS 部署指南（Ubuntu/Debian，git clone + 自行编译）

本文档假设：VPS 上不需要对外暴露 HTTPS，只通过 SSH 隧道从本机访问控制台。

## 1. VPS 上安装 Go

推荐直接用 `apt` 安装（省心，但版本可能不是最新）：

```bash
sudo apt update
sudo apt install -y golang-go git
go version
```

如果 apt 源里的 Go 版本太旧（低于 go.mod 里要求的版本，编译时会提示），改用官方最新版：

```bash
# 访问 https://go.dev/dl/ 确认最新版本号，或用国内镜像：
wget https://mirrors.aliyun.com/golang/go1.25.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
go version   # 确认能看到版本号
```

## 2. 创建专用运行用户（安全起见，不用 root 跑）

```bash
sudo useradd -r -s /bin/false -d /opt/gridbot gridbot
sudo mkdir -p /opt/gridbot
sudo chown gridbot:gridbot /opt/gridbot
```

## 3. 拉取代码并编译

```bash
cd /opt/gridbot
sudo -u gridbot git clone https://github.com/你的用户名/gridbot.git src
cd src
sudo -u gridbot bash -c '
  export PATH=$PATH:/usr/local/go/bin
  export GOPROXY=https://goproxy.cn,direct
  go mod tidy
  go build -o /opt/gridbot/gridbot .
'
sudo -u gridbot cp src/config.example.json /opt/gridbot/config.json
```

编辑 `/opt/gridbot/config.json`，把 `listen_addr` 改成 `"127.0.0.1:3000"`（仅本机监听，配合下面的SSH隧道使用，不对公网暴露）：

```bash
sudo -u gridbot nano /opt/gridbot/config.json
```

## 4. 配置 systemd 服务（开机自启、崩溃自动重启）

```bash
sudo cp /opt/gridbot/src/deploy/gridbot.service /etc/systemd/system/gridbot.service
sudo systemctl daemon-reload
sudo systemctl enable gridbot
sudo systemctl start gridbot
```

查看运行状态和日志：

```bash
sudo systemctl status gridbot
sudo journalctl -u gridbot -f    # 实时看日志，Ctrl+C退出
```

## 5. 从本机通过 SSH 隧道访问控制台

在你自己的 Windows 电脑上（不是VPS上）执行：

```cmd
ssh -L 3000:127.0.0.1:3000 你的VPS用户名@你的VPS的IP
```

这条命令会：把你本机的 3000 端口，通过 SSH 加密隧道转发到 VPS 上的 127.0.0.1:3000。

**保持这个 SSH 窗口不要关**，然后在浏览器打开：

```
http://127.0.0.1:3000
```

看到的就是 VPS 上跑的那个 GridBot 控制台，流量全程走 SSH 加密隧道，不需要额外配置 HTTPS 证书。

如果不想一直开着这个终端窗口，可以后台运行隧道（Windows 用 PuTTY 的话在会话设置里配置端口转发；用原生 `ssh` 命令则加 `-fN` 参数在后台保持连接）：

```cmd
ssh -fN -L 3000:127.0.0.1:3000 你的VPS用户名@你的VPS的IP
```

## 6. 以后更新代码怎么重新部署

```bash
cd /opt/gridbot/src
sudo -u gridbot git pull
sudo -u gridbot bash -c '
  export PATH=$PATH:/usr/local/go/bin
  export GOPROXY=https://goproxy.cn,direct
  go mod tidy
  go build -o /opt/gridbot/gridbot .
'
sudo systemctl restart gridbot
```

## 7. 数据库和日志在哪

- 数据库文件：`/opt/gridbot/gridbot.db`（账户密码、交易所API Key、网格配置、成交记录全在这里，**一定要定期备份**，且不要传到公开地方）
- 应用日志：`sudo journalctl -u gridbot`

## 8. 防火墙提醒

因为 GridBot 只监听 `127.0.0.1`（VPS 自己内部），即使你的 VPS 防火墙完全没配置、3000端口理论上也是从公网访问不到的（`127.0.0.1` 只接受本机发起的连接）。SSH 端口（默认22）该怎么加固还是要正常加固（改端口/禁密码登录用密钥/装 fail2ban 之类），这个和 GridBot 本身无关，是VPS通用的安全基本功。