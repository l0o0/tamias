# Tamias 前端

Vue 3、TypeScript 与 Vite 构建的桌面界面，调用本地 `/api` handler。开发服务代理到 `127.0.0.1:9240`。

```sh
npm install
npm run dev
npm run typecheck
npm run build
```

开发预览需要同时启动本地服务；前端所有 API 请求携带 `X-Tami-Client: desktop`。正式桌面环境通过同源 handler 提供同一组接口。
