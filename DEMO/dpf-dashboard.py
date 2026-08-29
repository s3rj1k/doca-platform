#!/usr/bin/env python3
"""Live DPF / DPU lifecycle dashboard (rich TUI).

  ./dpf-dashboard.py            # full-screen live TUI (default 3s refresh)
  ./dpf-dashboard.py once       # render a single frame and exit
  DPF_REFRESH=5 ./dpf-dashboard.py

Every section autoscrolls: if its rows do not fit, it pages through them one
page per refresh and shows the range in the panel title, e.g. "5-8/16".
"""
import base64
import json
import math
import os
import subprocess
import sys
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor

from rich.console import Console, Group
from rich.layout import Layout
from rich.live import Live
from rich.panel import Panel
from rich.table import Table
from rich.text import Text

NS = os.environ.get("DPF_NS", "dpf-operator-system")
REFRESH = int(os.environ.get("DPF_REFRESH", "3"))
REBOOT_ANN = "provisioning.dpu.nvidia.com/dpunode-external-reboot-required"
CTRL_NS = ["dpf-operator-system", "cert-manager", "node-feature-discovery", "kube-system"]

console = Console()


def sh(cmd, timeout=15):
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout).stdout
    except Exception:
        return ""


def kjson(args, kubeconfig=None):
    cmd = ["kubectl"] + (["--kubeconfig", kubeconfig] if kubeconfig else []) + ["-n", NS] + args + ["-o", "json"]
    try:
        return json.loads(sh(cmd) or "{}")
    except Exception:
        return {}


def bmc_get(ip, pw, path):
    try:
        return json.loads(sh(["curl", "-sk", "-u", f"root:{pw}", f"https://{ip}{path}"], timeout=6) or "{}")
    except Exception:
        return {}


def bmc_creds():
    pw = ""
    try:
        pw = base64.b64decode(kjson(["get", "secret", "bmc-shared-password"])["data"]["password"]).decode().strip()
    except Exception:
        pass
    ip = ""
    for it in kjson(["get", "dpudevice"]).get("items", []):
        ip = it.get("spec", {}).get("bmcIp", "") or ip
    return ip, pw


# ---------- autoscroll ----------
_SCROLL = {}  # key -> current page index


def paginate(key, items, win):
    """Return (visible_slice, indicator). Pages one screenful per call, in order,
    wrapping at the end. indicator is None when everything already fits."""
    win = max(1, win)
    total = len(items)
    if total <= win:
        _SCROLL[key] = 0
        return items, None
    pages = math.ceil(total / win)
    p = _SCROLL.get(key, 0) % pages
    start, end = p * win, min(p * win + win, total)
    _SCROLL[key] = (p + 1) % pages
    return items[start:end], f"{start + 1}-{end}/{total}"


def paginate_tall(key, rows, win, width):
    """paginate() for panels built from Text rows, which wrap at narrow widths.
    Pages by rendered lines so a wrapped row is never silently cropped."""
    win = max(1, win)
    heights = [max(1, len(r.wrap(console, width))) if isinstance(r, Text) else 1 for r in rows]
    if sum(heights) <= win:
        _SCROLL[key] = 0
        return rows, None
    pages, cur, used = [], [], 0
    for i, h in enumerate(heights):
        if cur and used + h > win:
            pages.append(cur)
            cur, used = [], 0
        cur.append(i)
        used += h
    if cur:
        pages.append(cur)
    p = _SCROLL.get(key, 0) % len(pages)
    _SCROLL[key] = (p + 1) % len(pages)
    idx = pages[p]
    return [rows[i] for i in idx], f"{idx[0] + 1}-{idx[-1] + 1}/{len(rows)}"


def fit(budget, desired, floors):
    """Make the panel sizes in a column sum to exactly budget. Overflow is
    taken from the bottom panel up, spare rows go to the last panel."""
    sizes = list(desired)
    over = sum(sizes) - budget
    while over > 0:
        # Always trim whichever panel sits furthest above its floor, so one
        # long table cannot starve its neighbour down to a single row.
        i = max(range(len(sizes)), key=lambda j: sizes[j] - floors[j])
        if sizes[i] <= floors[i]:
            break
        sizes[i] -= 1
        over -= 1
    if over < 0:
        sizes[-1] -= over
    return sizes


def titled(base, indicator):
    return f"[bold]{base}" + (f"  [dim]{indicator} ▾[/dim]" if indicator else "")


# ---------- data ----------
_FW = {"ts": 0, "rows": []}


def _fw_rank(name):
    """Live components first, then staged updates, then the golden images."""
    return (name.startswith("golden_image"), name.endswith("_pending"), name)


def fw_versions(ip, pw):
    """Enumerate the BMC inventory rather than guessing component names, the
    hardcoded list missed ERoT, UEFI, NODE, SYS_IMAGE and the golden images."""
    if time.time() - _FW["ts"] < 60 and _FW["rows"]:
        return _FW["rows"]
    inv = bmc_get(ip, pw, "/redfish/v1/UpdateService/FirmwareInventory")
    urls = [m["@odata.id"] for m in inv.get("Members", []) if "@odata.id" in m]
    if not urls:
        return _FW["rows"]
    # Sequential reads of 17 components would stall a frame, so fan them out.
    with ThreadPoolExecutor(max_workers=8) as ex:
        got = ex.map(lambda u: (u.rsplit("/", 1)[-1], bmc_get(ip, pw, u).get("Version", "")), urls)
    rows = sorted(((n, v or "-") for n, v in got), key=lambda r: _fw_rank(r[0]))
    if rows:
        _FW.update(ts=time.time(), rows=rows)
    return rows


def image_version(img):
    """Tag out of an image ref, tolerating a digest and a registry port."""
    tail = img.split("@", 1)[0].rsplit("/", 1)[-1]
    return tail.rsplit(":", 1)[-1] if ":" in tail else "latest"


def host_uptime():
    """Uptime of the host this runs on. A power cycle resets it, so it doubles
    as proof the manual reboot the DPU waits on actually happened."""
    try:
        secs = int(float(open("/proc/uptime").read().split()[0]))
    except Exception:
        return "?"
    d, rem = divmod(secs, 86400)
    h, rem = divmod(rem, 3600)
    m = rem // 60
    span = f"{d}d {h}h {m}m" if d else (f"{h}h {m}m" if h else f"{m}m")
    booted = time.strftime("%Y-%m-%d %H:%M", time.localtime(time.time() - secs))
    return f"{span} since {booted}"


def power_cycle_state(dpus, nodes):
    waiting = [d["metadata"]["name"] for d in dpus
               for c in d.get("status", {}).get("conditions", [])
               if c.get("type") == "Rebooted" and c.get("status") != "True" and "PowerCycle" in (c.get("reason") or "")]
    annotated = [n["metadata"]["name"] for n in nodes
                 if (n["metadata"].get("annotations") or {}).get(REBOOT_ANN) == "true"]
    return waiting, annotated


def pod_rows():
    """One row per pod, carrying the version of its first container. Replaces
    the old workload table, which restated every pod under another name."""
    rows = []
    for p in kjson(["get", "pods", "-A"]).get("items", []):
        ns, name = p["metadata"]["namespace"], p["metadata"]["name"]
        st = p.get("status", {})
        cs = st.get("containerStatuses", []) or []
        imgs = [c["image"] for c in p["spec"].get("containers", [])]
        ver = image_version(imgs[0]) if imgs else "?"
        ready, total = sum(1 for c in cs if c.get("ready")), len(imgs)
        waiting = next((c["state"]["waiting"].get("reason") for c in cs
                        if "waiting" in (c.get("state") or {})), None)
        phase = st.get("phase", "?")
        ok = bool(total) and ready == total and phase == "Running" and not waiting
        rows.append((ns, name, ver, f"{ready}/{total}", waiting or phase, ok))
    rows.sort(key=lambda x: (CTRL_NS.index(x[0]) if x[0] in CTRL_NS else 99, x[0], x[1]))
    return rows


# ---------- panels ----------
def banner_panel(waiting, annotated):
    if waiting or annotated:
        who = ", ".join(waiting or annotated)
        body = Text.assemble(
            ("  ACTION REQUIRED: SERVER POWER CYCLE NEEDED  \n", "bold white on red"),
            ("DPU flashed new firmware and is halted. ", "yellow"), (f"Waiting: {who}\n", "bold"),
            ("Power-cycle the server, then run ", "yellow"), ("./dpf-release-reboot.sh", "bold cyan"))
        return Panel(body, border_style="red", title="[bold red]!! ACTION !!", padding=(0, 1))
    return Panel(Text("No action pending — provisioning nominal.", style="green"), border_style="green", padding=(0, 1))


def operator_summary(cfg):
    ready = next((c["status"] for c in cfg.get("status", {}).get("conditions", []) if c["type"] == "Ready"), "?")
    iface = json.dumps(cfg.get("spec", {}).get("provisioningController", {}).get("installInterface", {}))
    iface = "redfish" if "Redfish" in iface else ("hostAgent" if "HostAgent" in iface else "hostAgent(default)")
    return ready, iface


def inputs_rows():
    rows = []
    bfb = ", ".join(f"{i['metadata']['name']}={i.get('status', {}).get('phase', '?')}"
                    for i in kjson(["get", "bfb"]).get("items", []))
    disc = ", ".join(f"{i['metadata']['name']} scan={i.get('status', {}).get('lastScanTime', '?')}"
                     for i in kjson(["get", "dpudiscovery"]).get("items", []))
    rows.append(Text.assemble(("BFB: ", "dim"), (bfb or "none", "")))
    rows.append(Text.assemble(("Discovery: ", "dim"), (disc or "none", "")))
    for d in kjson(["get", "dpudevice"]).get("items", []):
        sp = d.get("spec", {})
        rows.append(Text.assemble(("Device: ", "dim"), (f"{d['metadata']['name']}  bmcIp={sp.get('bmcIp', '?')}:{sp.get('bmcPort', '?')}", "")))
    return rows


def inputs_panel(rows, win, width):
    view, ind = paginate_tall("in", rows, win, width)
    return Panel(Group(*view), title=titled("INPUTS", ind), border_style="blue", padding=(0, 1))


def dpu_rows(dpus, ip, pw):
    rows = []
    if not dpus:
        rows.append(Text("(no DPU objects yet)", style="dim"))
    for d in dpus:
        s = d.get("status", {})
        ph = s.get("phase", "?")
        col = {"Ready": "green", "Error": "red"}.get(ph, "yellow")
        rows.append(Text.assemble((d["metadata"]["name"] + "  ", "bold"), (ph, f"bold {col}")))
        # Every condition, not just the unmet ones. The panel scrolls, so the
        # green ones cost nothing and show how far provisioning actually got.
        for c in sorted(s.get("conditions", []), key=lambda c: c.get("status") == "True"):
            typ, stat, reason = c.get("type"), c.get("status"), c.get("reason") or ""
            msg = (c.get("message") or "")[:90]
            if typ == "Rebooted" and stat != "True":
                rows.append(Text(f"  Rebooted={stat} {reason}  <== power cycle", style="bold red"))
            elif stat != "True":
                rows.append(Text(f"  {typ}={stat} {reason} {msg}", style="yellow"))
            else:
                rows.append(Text(f"  {typ}={stat} {reason} {msg}", style="green"))
    if ip and pw:
        ps = bmc_get(ip, pw, "/redfish/v1/Systems/Bluefield").get("PowerState", "unreachable")
        pcol = "red" if ps in ("Paused", "Off") else "green"
        rows.append(Text.assemble(("BMC ", "dim"), (f"{ip} ", ""), ("Power=", "dim"), (ps, f"bold {pcol}")))
    return rows


def dpu_panel(rows, cfg, win, width):
    view, ind = paginate_tall("dpu", rows, win, width)
    ready, iface = operator_summary(cfg)
    base = f"DPU  operator ready={ready} install={iface}"
    return Panel(Group(*view), title=titled(base, ind), border_style="blue", padding=(0, 1))


def cluster_rows():
    kc = ""
    try:
        kc = base64.b64decode(kjson(["get", "secret", "dpu-cluster-1-admin-kubeconfig"])["data"]["super-admin.conf"]).decode()
    except Exception:
        pass
    rows = []
    if kc:
        tf = tempfile.NamedTemporaryFile("w", suffix=".conf", delete=False)
        tf.write(kc); tf.close()
        try:
            os.chmod(tf.name, 0o600)
            for line in sh(["kubectl", "--kubeconfig", tf.name, "get", "nodes", "-o", "wide", "--no-headers"], timeout=8).splitlines():
                f = line.split()
                if len(f) >= 5:
                    rows.append((f[0], f[1], f[2], f[3], f[4], f[5] if len(f) > 5 else "-"))
        finally:
            os.remove(tf.name)
    return rows


def cluster_panel(rows, win):
    view, ind = paginate("cluster", rows, win)
    t = Table(box=None, show_header=True, header_style="dim", expand=True)
    for c in ("NODE", "STATUS", "ROLE", "AGE", "VERSION", "IP"):
        t.add_column(c, overflow="fold")
    if not rows:
        t.add_row("(no nodes / kubeconfig)", "", "", "", "", "")
    for n, stt, role, age, ver, ip in view:
        t.add_row(n, Text(stt, style="green" if stt == "Ready" else "yellow"), role, age, ver, ip)
    return Panel(t, title=titled("DPU CLUSTER (dpu-cluster-1)", ind), border_style="blue")


def pods_panel(rows, win):
    """Every pod in the cluster with the version it is running, full width."""
    view, ind = paginate("pods", rows, win)
    t = Table(box=None, show_header=True, header_style="dim", expand=True)
    t.add_column("NS", overflow="fold", style="magenta"); t.add_column("POD", overflow="fold")
    t.add_column("VER", overflow="fold"); t.add_column("RDY", justify="right")
    t.add_column("STATUS", overflow="fold")
    if not rows:
        t.add_row("-", "(none)", "", "", "")
    prev = None
    for ns, name, ver, rdy, st, ok in view:
        col = "green" if ok else "yellow"
        t.add_row("" if ns == prev else ns, name, Text(ver, style="cyan"),
                  Text(rdy, style=col), Text(st, style=col))
        prev = ns
    return Panel(t, title=titled("PODS & VERSIONS", ind), border_style="green")


def firmware_panel(ip, pw, win):
    if not (ip and pw):
        return Panel(Text("(BMC creds unavailable)", style="dim"), title="[bold]DPU FIRMWARE", border_style="green")
    view, ind = paginate("fw", fw_versions(ip, pw), win)
    t = Table(box=None, show_header=False, expand=True)
    t.add_column("comp"); t.add_column("ver", overflow="fold")
    for comp, v in view:
        t.add_row(Text(comp, style="dim"), Text(v, style="cyan"))
    return Panel(t, title=titled("DPU FIRMWARE (BMC inventory)", ind), border_style="green")


def hostnet_rows():
    try:
        addrs = json.loads(sh(["ip", "-j", "addr"]) or "[]")
    except Exception:
        addrs = []
    # Every real interface, not a hardcoded shortlist. Only the per-pod veth
    # pairs are dropped, they are churn and would bury everything else.
    skip = ("cali", "veth", "lxc", "tunl")
    rows = []
    for a in addrs:
        n = a.get("ifname", "")
        if not n or any(n.startswith(p) for p in skip):
            continue
        st = a.get("operstate", "?")
        v4 = " ".join(f"{i['local']}/{i['prefixlen']}" for i in a.get("addr_info", []) if i.get("family") == "inet")
        rows.append((n, st, v4))
    try:
        rows.append(("members", "", ", ".join(os.listdir("/sys/class/net/br-dpu/brif"))))
    except Exception:
        pass
    for line in (sh(["ip", "route"]) or "").splitlines():
        if line.startswith("default"):
            rows.append(("route", "", line)); break
    return rows


def hostnet_panel(win):
    view, ind = paginate("net", hostnet_rows(), win)
    t = Table(box=None, show_header=False, expand=True)
    t.add_column("if"); t.add_column("state"); t.add_column("addr", overflow="fold")
    for n, st, addr in view:
        col = "green" if st == "UP" else ("dim" if st in ("", "DOWN", "UNKNOWN") else "yellow")
        t.add_row(Text(n, style="dim" if n in ("members", "route") else ""), Text(st, style=col), addr)
    return Panel(t, title=titled("HOST NETWORK", ind), border_style="green")


def build():
    dpus = kjson(["get", "dpu"]).get("items", [])
    nodes = kjson(["get", "dpunode"]).get("items", [])
    cfg = kjson(["get", "dpfoperatorconfig", "dpfoperatorconfig"])
    ip, pw = bmc_creds()
    waiting, annotated = power_cycle_state(dpus, nodes)
    prows = pod_rows()


    term_h = console.size.height
    # Inner width of the narrow column, needed to page Text rows by real height.
    right_inner = max(20, (console.size.width - console.size.width * 3 // 5) - 4)
    banner_h = 5 if (waiting or annotated) else 3
    ov, ovh = 3, 4                               # border+title ; +header row for tables

    # Wide tables span both columns, pods across the top and the node list
    # across the bottom. Only the narrower panels sit inside the two columns.
    irows = inputs_rows()
    drows = dpu_rows(dpus, ip, pw)
    crows = cluster_rows()
    frows = fw_versions(ip, pw) if (ip and pw) else []
    nrows = hostnet_rows()

    in_h = min(8, max(1, len(irows)) + ov)
    dpu_h = min(14, max(1, len(drows)) + ov)
    cl_h = min(8, max(1, len(crows)) + ovh)
    fw_h = min(12, max(1, len(frows)) + ov)
    net_h = min(12, max(1, len(nrows)) + ov)

    body = term_h - 3 - banner_h - cl_h          # rows left for pods plus the columns
    # The taller column sets the shared height, capped so the pods panel keeps
    # a usable share. fit() then trims or pads each column to land exactly.
    # Capped at a share of the body as well, otherwise the two columns starve
    # the pods table down to a couple of rows on a mid-sized terminal.
    cols_h = max(8, min(max(fw_h + net_h, in_h + dpu_h), body - 6, int(body * 0.62)))
    fw_h, net_h = fit(cols_h, [fw_h, net_h], [ov + 1, ov + 1])
    in_h, dpu_h = fit(cols_h, [in_h, dpu_h], [ov + 1, ov + 1])
    top_h = max(6, body - cols_h)

    root = Layout()
    root.split_column(
        Layout(Panel(Text(f"DPF / DPU LIFECYCLE   {time.strftime('%Y-%m-%d %H:%M:%S %Z')}   "
                          f"up {host_uptime()}   refresh {REFRESH}s   ns={NS}",
                          style="bold cyan"), border_style="cyan"), name="head", size=3),
        Layout(banner_panel(waiting, annotated), name="banner", size=banner_h),
        Layout(pods_panel(prows, top_h - ovh), name="top", size=top_h),
        Layout(name="body"),
        Layout(cluster_panel(crows, cl_h - ovh), name="cl", size=cl_h),
    )
    root["body"].split_row(Layout(name="left", ratio=3), Layout(name="right", ratio=2))
    root["left"].split_column(
        Layout(firmware_panel(ip, pw, fw_h - ov), name="fw", size=fw_h),
        Layout(hostnet_panel(net_h - ov), name="net", size=net_h),
    )
    root["right"].split_column(
        Layout(inputs_panel(irows, in_h - ov, right_inner), name="in", size=in_h),
        Layout(dpu_panel(drows, cfg, dpu_h - ov, right_inner), name="dpu", size=dpu_h),
    )
    return root


def main():
    if len(sys.argv) > 1 and sys.argv[1] == "once":
        console.print(build())
        return
    with Live(build(), console=console, screen=True, refresh_per_second=4) as live:
        try:
            while True:
                time.sleep(REFRESH)
                live.update(build())
        except KeyboardInterrupt:
            pass


if __name__ == "__main__":
    main()
