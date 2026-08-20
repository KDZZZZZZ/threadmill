# Threadmill

Threadmill 是轻量级 Agent OS。

## 凭据配置

项目的 `threadmill.yaml` 只保存凭据名：

```yaml
llm:
  credential: opencode
```

密钥统一保存在用户目录的 `~/.threadmill/credentials.yaml`，同名字段对应项目中的凭据名：

```yaml
opencode: sk-your-key
```

在 Unix 系统上，该文件必须只有当前用户可访问：

```sh
mkdir -p ~/.threadmill
chmod 700 ~/.threadmill
chmod 600 ~/.threadmill/credentials.yaml
```
