import { Hono } from "hono";

type Bindings = {
  ENVIRONMENT: string;
  ASSETS: Fetcher;
};

const app = new Hono<{ Bindings: Bindings }>();

app.get("/health", (c) =>
  c.json({ name: "CeraaSite", status: "online", host: "ceraa.auxlo.xyz" })
);

app.all("*", (c) => c.env.ASSETS.fetch(c.req.raw));

export default app;
