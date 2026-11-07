#!/usr/bin/env node
import { login, api, done, fail } from "./lib.mjs";

const name = (process.env.SB_INPUT_NAME || "").trim();
const domain = (process.env.SB_INPUT_DOMAIN || "").trim().replace(/^https?:\/\//, "").replace(/\/$/, "");
if (!name || !domain) fail("create_website needs input name and domain, e.g. --input name=Shop --input domain=shop.example.com");
const token = await login();
const site = await api(token, "/api/websites", { name, domain });
const script = process.env.SB_APP_URL + "/script.js";
done({
  website_id: site.id, name: site.name, domain: site.domain, script_url: script,
  snippet: `<script defer src="${script}" data-website-id="${site.id}"></script>`,
  next: "Add the snippet inside <head> of the site (for a Vite/React app: index.html), then redeploy that app.",
});
