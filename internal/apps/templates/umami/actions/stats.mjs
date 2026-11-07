#!/usr/bin/env node
import { login, api, num, done } from "./lib.mjs";

let hours = parseInt(process.env.SB_INPUT_HOURS || "24", 10);
if (!(hours > 0 && hours <= 24 * 90)) hours = 24;
const token = await login();
const sites = (await api(token, "/api/websites")).data || [];
const end = Date.now();
const websites = [];
for (const w of sites) {
  const st = await api(token, `/api/websites/${w.id}/stats?startAt=${end - hours * 3600000}&endAt=${end}`);
  websites.push({ name: w.name, domain: w.domain, website_id: w.id,
    pageviews: num(st.pageviews), visitors: num(st.visitors), visits: num(st.visits) });
}
done({ hours, websites });
