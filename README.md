家庭工作台

## 文档

- [部署手册.md](部署手册.md) —— 怎么装、怎么开、怎么备份、公网 HTTPS、升级与排障
- [家庭工作台-设计方案.md](家庭工作台-设计方案.md) —— 数据模型、接口契约与鉴权设计

## 快速开始

```bash
cp .env.example .env && $EDITOR .env    # 设置 FW_AUTH_PASSWORD
make local                              # 本机运行，打开 http://localhost:8080
# 或容器方式：make up（Podman 用 make up COMPOSE="podman compose"）
```
