#!/usr/bin/env node
import { base, login, api, num, done, fail } from "./lib.mjs";

const token = await login();
const sites = (await api(token, "/api/websites")).data || [];
let id = (process.env.SB_INPUT_WEBSITE_ID || "").trim();
const site = id ? sites.find((w) => w.id === id) : sites.length === 1 ? sites[0] : null;
if (!site) fail(`pass --input website_id=<id> (there are ${sites.length} websites; see the stats action)`);
id = site.id;
const count = async () => {
  const end = Date.now() + 60000;
  const st = await api(token, `/api/websites/${id}/stats?startAt=${end - 86400000}&endAt=${end}`);
  return num(st.pageviews);
};
const before = await count();
const r = await fetch(base + "/api/send", {
  method: "POST",
  headers: {
    "content-type": "application/json",
    "user-agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36 SlicedBread-test",
  },
  body: JSON.stringify({ type: "event", payload: {
    website: id, hostname: site.domain, url: "/sb-test-event", title: "SlicedBread test event", language: "en-US", screen: "1920x1080",
  } }),
});
if (!r.ok) fail(`umami rejected the event: HTTP ${r.status}`);
let after = before;
for (let i = 0; i < 10 && after <= before; i++) {
  await new Promise((res) => setTimeout(res, 1000));
  after = await count();
}
done({ website_id: id, domain: site.domain, pageviews_before: before, pageviews_after: after, counted: after > before,
  note: "one page view at /sb-test-event was added to this website's stats" });
