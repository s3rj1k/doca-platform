#!/usr/bin/env python3
"""Live DPF and DPU lifecycle dashboard (rich TUI).

  set -a && source ./demo.env       load NS, DPUCLUSTER, REFRESH, DPU_PCI
  ./dpf-dashboard.py                full-screen live TUI
  ./dpf-dashboard.py once           render a single frame and exit

Every section autoscrolls. If its rows do not fit it pages through them one page
per refresh and shows the range in the panel title, e.g. "5-8/16".

This is the host-trusted rshim demo, so there is no BMC anywhere in it. No
Redfish reads, no firmware inventory, no manual power cycle banner. DPF reboots
the host itself through the host agent. Read only, it changes nothing.
"""
import base64
import json
import math
import os
import subprocess
import sys
import tempfile
import time

from rich.console import Console, Group
from rich.layout import Layout
from rich.live import Live
from rich.panel import Panel
from rich.table import Table
from rich.text import Text


def _env(name):
    val = os.environ.get(name)
    if not val:
        sys.exit(f"{name} is unset. Source demo.env first with  set -a && source ./demo.env")
    return val


NS = _env("NS")
REFRESH = int(_env("REFRESH"))
DPU_PCI = _env("DPU_PCI")

# The cluster manager gives the DPUCluster and the k0smotron Cluster the same
# name and namespace, so one name covers both.
DPUCLUSTER = _env("DPUCLUSTER")

console = Console()


def sh(cmd, timeout=15):
    """Run a command and return stdout, swallowing every failure.

    check is False on purpose, a non-zero exit still has usable stdout.
    """
    try:
        return subprocess.run(cmd, capture_output=True, text=True,
                              timeout=timeout, check=False).stdout
    except (OSError, subprocess.SubprocessError):
        return ""


def kjson(args, kubeconfig=None):
    """kubectl -o json as a dict, or an empty dict on any failure."""
    cmd = (["kubectl"] + (["--kubeconfig", kubeconfig] if kubeconfig else [])
           + ["-n", NS] + args + ["-o", "json"])
    try:
        return json.loads(sh(cmd) or "{}")
    except json.JSONDecodeError:
        return {}


# ---------- autoscroll ----------
_SCROLL = {}


def paginate(key, items, win):
    """Return (visible_slice, indicator), paging one screenful per call.

    indicator is None when everything already fits.
    """
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
    """paginate() for panels built from Text rows, which wrap when narrow.

    Pages by rendered lines so a wrapped row is never silently cropped.
    """
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


def fit(budget, desired, floors, grow=-1):
    """Make the panel sizes in a column sum to exactly budget.

    Overflow comes off whichever panel is furthest above its floor, and spare
    rows go to grow rather than always to the last panel.
    """
    sizes = list(desired)
    over = sum(sizes) - budget
    while over > 0:
        i = max(range(len(sizes)), key=lambda j: sizes[j] - floors[j])
        if sizes[i] <= floors[i]:
            break
        sizes[i] -= 1
        over -= 1
    if over < 0:
        sizes[grow] -= over
    return sizes


def wrapped_h(rows, width):
    """Rendered height of Text rows at a width, not the raw row count.

    Long condition messages wrap, so counting rows under-requests space.
    """
    return sum(max(1, len(r.wrap(console, width))) if isinstance(r, Text) else 1
               for r in rows)


def titled(base, indicator):
    return f"[bold]{base}" + (f"  [dim]{indicator} ▾[/dim]" if indicator else "")


# ---------- data ----------
def host_uptime():
    """Uptime of the host this runs on.

    DPF reboots the host through the host agent, so this moving is a signal.
    """
    try:
        with open("/proc/uptime", encoding="utf-8") as fh:
            secs = int(float(fh.read().split()[0]))
    except (OSError, ValueError, IndexError):
        return "?"
    d, rem = divmod(secs, 86400)
    h, rem = divmod(rem, 3600)
    m = rem // 60
    span = f"{d}d {h}h {m}m" if d else (f"{h}h {m}m" if h else f"{m}m")
    booted = time.strftime("%Y-%m-%d %H:%M", time.localtime(time.time() - secs))
    return f"{span} since {booted}"


def rshim_state():
    """Whether DPF owns the rshim channel.

    BAR2 must exist and exactly one rshim should hold it, DPF's own.
    """
    bar2 = os.path.exists(f"/sys/bus/pci/devices/0000:{DPU_PCI}/resource2")
    procs = [ln for ln in (sh(["ps", "-eo", "pid,args"]) or "").splitlines()
             if "rshim" in ln and "ps -eo" not in ln]
    masked = (sh(["systemctl", "is-enabled", "rshim"]) or "").strip() == "masked"
    return bar2, len(procs), masked


# ---------- panels ----------
def banner_rows(dpus):
    """Every unmet DPU condition, so none is hidden behind the first."""
    rows = []
    for d in dpus:
        for c in d.get("status", {}).get("conditions", []):
            if c.get("status") != "True":
                rows.append(Text.assemble(
                    (d["metadata"]["name"] + "  ", "bold"),
                    (f"{c.get('type')}={c.get('reason') or ''} ", "bold yellow"),
                    ((c.get("message") or "").replace("\n", " ")[:90], "")))
    return rows


def banner_panel(rows, win, width):
    if not rows:
        return Panel(Text("No blocking condition, provisioning nominal.", style="green"),
                     border_style="green", padding=(0, 1))
    view, ind = paginate_tall("banner", rows, win, width)
    return Panel(Group(*view), border_style="yellow",
                 title=titled("BLOCKED ON", ind), padding=(0, 1))


def inputs_rows():
    rows = []
    bfb = ", ".join(f"{i['metadata']['name']}={i.get('status', {}).get('phase', '?')}"
                    for i in kjson(["get", "bfb"]).get("items", []))
    rows.append(Text.assemble(("BFB    ", "dim"), (bfb or "none", "")))

    bar2, nproc, masked = rshim_state()
    ok = bar2 and nproc == 1 and masked
    detail = f"BAR2={'yes' if bar2 else 'NO'} procs={nproc} host={'masked' if masked else 'NOT masked'}"
    rows.append(Text.assemble(("rshim  ", "dim"), (detail, "green" if ok else "yellow")))

    for d in kjson(["get", "dpudevice"]).get("items", []):
        sp, st = d.get("spec", {}), d.get("status", {})
        rows.append(Text.assemble(
            ("Device ", "dim"),
            (f"{d['metadata']['name']} serial={sp.get('serialNumber', '?')} ", ""),
            (f"type={st.get('dpuType', '?')}", "green" if st.get("dpuType") not in (None, "Unknown") else "yellow")))

    for n in kjson(["get", "dpunode"]).get("items", []):
        method = ",".join((n.get("spec", {}).get("nodeRebootMethod") or {}).keys()) or "?"
        rows.append(Text.assemble(("Node   ", "dim"),
                                  (f"{n['metadata']['name']} reboot={method}", "")))
    return rows


def inputs_panel(rows, win, width):
    view, ind = paginate_tall("in", rows, win, width)
    return Panel(Group(*view), title=titled("INPUTS", ind), border_style="blue", padding=(0, 1))


def controlplane_rows():
    """The hosted control plane, which the static manager never had.

    Each line here is somewhere bring up stalls before a DPU can join.
    """
    rows = []
    dc = kjson(["get", "dpucluster", DPUCLUSTER])
    phase = dc.get("status", {}).get("phase", "-") if dc else "-"
    col = {"Ready": "green", "Failed": "red", "-": "dim"}.get(phase, "yellow")
    rows.append(Text.assemble(("DPUCluster ", "dim"), (f"{DPUCLUSTER} ", ""), (phase, f"bold {col}")))

    for c in sorted(dc.get("status", {}).get("conditions", []), key=lambda c: c.get("status") == "True"):
        stat = c.get("status")
        rows.append(Text(f"  {c.get('type')}={stat} {c.get('reason') or ''} {(c.get('message') or '')[:70]}",
                         style="green" if stat == "True" else "yellow"))

    km = kjson(["get", "clusters.k0smotron.io", DPUCLUSTER])
    if km:
        spec = km.get("spec", {})
        svc = spec.get("service", {})
        rows.append(Text.assemble(("k0smotron  ", "dim"),
                                  (f"version={spec.get('version', '?')} ", ""),
                                  (f"{svc.get('type', '?')}/{svc.get('apiPort', '?')}", "cyan")))
    else:
        rows.append(Text("k0smotron  no Cluster object yet", style="dim"))

    # WaitForFirstConsumer means Pending is normal until the etcd pod schedules.
    for p in kjson(["get", "pvc"]).get("items", []):
        ph = p.get("status", {}).get("phase", "?")
        cap = p.get("status", {}).get("capacity", {}).get("storage", "?")
        rows.append(Text.assemble(("PVC        ", "dim"), (f"{p['metadata']['name']} {cap} ", ""),
                                  (ph, "green" if ph == "Bound" else "yellow")))
    return rows


def controlplane_panel(rows, win, width):
    view, ind = paginate_tall("cp", rows, win, width)
    return Panel(Group(*view), title=titled("HOSTED CONTROL PLANE", ind),
                 border_style="blue", padding=(0, 1))


def dpu_rows(dpus):
    rows = []
    if not dpus:
        rows.append(Text("(no DPU objects yet)", style="dim"))
    for d in dpus:
        s = d.get("status", {})
        ph = s.get("phase", "?")
        col = {"Ready": "green", "Error": "red"}.get(ph, "yellow")
        rows.append(Text.assemble((d["metadata"]["name"] + "  ", "bold"), (ph, f"bold {col}")))
        # Every condition, not just unmet ones. The panel scrolls, so the green
        # ones cost nothing and show how far provisioning actually got.
        for c in sorted(s.get("conditions", []), key=lambda c: c.get("status") == "True"):
            typ, stat, reason = c.get("type"), c.get("status"), c.get("reason") or ""
            msg = (c.get("message") or "")[:90]
            rows.append(Text(f"  {typ}={stat} {reason} {msg}",
                             style="green" if stat == "True" else "yellow"))
    return rows


def dpu_panel(rows, cfg, win, width):
    """The mode is flagged, not just printed.

    rshim needs host-trusted with installViaHostAgent, and zero-trust cannot
    reach it, so anything else here is a misconfiguration for this demo.
    """
    view, ind = paginate_tall("dpu", rows, win, width)
    spec = cfg.get("spec", {})
    ready = next((c["status"] for c in cfg.get("status", {}).get("conditions", [])
                  if c["type"] == "Ready"), "?")
    raw = json.dumps(spec.get("provisioningController", {}).get("installInterface", {}))
    iface = "hostAgent" if "HostAgent" in raw else ("redfish" if "Redfish" in raw else "?")
    mode = spec.get("deploymentMode", "?")
    ok = mode == "host-trusted" and iface == "hostAgent"
    tag = f"{mode}/{iface}" if ok else f"{mode}/{iface} NOT RSHIM"
    return Panel(Group(*view),
                 title=titled(f"DPU  operator={ready} {tag}", ind),
                 border_style="blue" if ok else "red", padding=(0, 1))


def _hcp_kubeconfig():
    """Admin kubeconfig for the hosted cluster, published by the manager."""
    try:
        secret = kjson(["get", "secret", f"{DPUCLUSTER}-admin-kubeconfig"])
        return base64.b64decode(secret["data"]["super-admin.conf"]).decode()
    except (KeyError, TypeError, ValueError):
        return ""


def hcp_kubectl(args, timeout=8):
    """Run kubectl against the hosted cluster, empty string if unreachable."""
    kc = _hcp_kubeconfig()
    if not kc:
        return ""
    with tempfile.NamedTemporaryFile("w", suffix=".conf", delete=False) as tf:
        tf.write(kc)
        path = tf.name
    try:
        os.chmod(path, 0o600)
        return sh(["kubectl", "--kubeconfig", path] + args, timeout=timeout)
    finally:
        os.remove(path)


def cluster_rows():
    """Nodes of the hosted DPU cluster, read through its published kubeconfig."""
    rows = []
    out = hcp_kubectl(["get", "nodes", "-o", "wide", "--no-headers"])
    for line in out.splitlines():
        f = line.split()
        if len(f) >= 5:
            rows.append((f[0], f[1], f[2], f[3], f[4], f[5] if len(f) > 5 else "-"))
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
    return Panel(t, title=titled(f"DPU WORKER NODES ({DPUCLUSTER})", ind), border_style="blue")


def operator_rows(cfg):
    """DPFOperatorConfig conditions, unmet first.

    An unmet one here is the reason everything downstream is waiting.
    """
    rows = []
    conds = cfg.get("status", {}).get("conditions", []) if cfg else []
    if not conds:
        rows.append(Text("no DPFOperatorConfig yet", style="dim"))
    for c in sorted(conds, key=lambda c: c.get("status") == "True"):
        stat = c.get("status")
        msg = (c.get("message") or "").replace("\n", " ")[:70]
        rows.append(Text(f"{c.get('type')}={stat} {c.get('reason') or ''} {msg}",
                         style="green" if stat == "True" else "yellow"))
    return rows


def operator_panel(rows, win, width):
    view, ind = paginate_tall("op", rows, win, width)
    return Panel(Group(*view), title=titled("DPF OPERATOR", ind),
                 border_style="green", padding=(0, 1))


def _pod_row(p, with_ns=False):
    """One pod as a (name, ready, phase, restarts, ok) tuple for the table."""
    st = p.get("status", {})
    cs = st.get("containerStatuses", []) or []
    ready = sum(1 for c in cs if c.get("ready"))
    total = len(p["spec"].get("containers", []))
    waiting = next((c["state"]["waiting"].get("reason") for c in cs
                    if "waiting" in (c.get("state") or {})), None)
    phase = waiting or st.get("phase", "?")
    restarts = sum(c.get("restartCount", 0) for c in cs)
    ok = total and ready == total and phase == "Running"
    name = p["metadata"]["name"]
    if with_ns:
        name = f"{p['metadata']['namespace']}/{name}"
    return (name, f"{ready}/{total}", phase, str(restarts), ok)


def component_rows():
    """Host side pods, only the DPF and k0smotron namespaces serve a DPU."""
    rows = [_pod_row(p) for p in kjson(["get", "pods", "-A"]).get("items", [])
            if p["metadata"]["namespace"] in (NS, "k0smotron")]
    rows.sort(key=lambda r: (r[4], r[0]))
    return rows


def hcp_pod_rows():
    """Pods inside the hosted DPU cluster, read through its kubeconfig."""
    out = hcp_kubectl(["get", "pods", "-A", "-o", "json"])
    try:
        items = json.loads(out or "{}").get("items", [])
    except json.JSONDecodeError:
        items = []
    rows = [_pod_row(p, with_ns=True) for p in items]
    rows.sort(key=lambda r: (r[4], r[0]))
    return rows


def pods_panel(rows, win, title, key):
    view, ind = paginate(key, rows, win)
    t = Table(box=None, show_header=True, header_style="dim", expand=True)
    t.add_column("POD", overflow="fold")
    t.add_column("RDY", justify="right")
    t.add_column("STATUS", overflow="fold")
    t.add_column("RS", justify="right")
    if not rows:
        t.add_row("(none)", "", "", "")
    for name, rdy, phase, rs, ok in view:
        col = "green" if ok else "yellow"
        t.add_row(name, Text(rdy, style=col), Text(phase, style=col),
                  Text(rs, style="yellow" if rs != "0" else "dim"))
    return Panel(t, title=titled(title, ind), border_style="green")


def build():
    dpus = kjson(["get", "dpu"]).get("items", [])
    cfg = kjson(["get", "dpfoperatorconfig", "dpfoperatorconfig"])

    term_h, term_w = console.size.height, console.size.width
    left_inner = max(20, (term_w * 3 // 5) - 4)
    right_inner = max(20, (term_w - term_w * 3 // 5) - 4)
    banner_h = 5
    ov, ovh = 3, 4

    brows = banner_rows(dpus)
    oprows = operator_rows(cfg)
    comprows = component_rows()
    hcprows = hcp_pod_rows()
    irows = inputs_rows()
    cprows = controlplane_rows()
    drows = dpu_rows(dpus)
    crows = cluster_rows()

    op_h = min(20, max(1, wrapped_h(oprows, left_inner)) + ov)
    comp_h = min(20, max(1, len(comprows)) + ovh)
    hcp_h = min(20, max(1, len(hcprows)) + ovh)
    in_h = min(10, max(1, wrapped_h(irows, right_inner)) + ov)
    cp_h = min(16, max(1, wrapped_h(cprows, right_inner)) + ov)
    dpu_h = min(14, max(1, wrapped_h(drows, right_inner)) + ov)
    cl_h = min(8, max(1, len(crows)) + ovh)

    body = max(16, term_h - 3 - banner_h - cl_h)
    # Spare rows go to the host COMPONENTS on the left and the control plane on
    # the right, the floors protect the other panels.
    op_h, comp_h, hcp_h = fit(body, [op_h, comp_h, hcp_h], [ov + 1, ovh + 1, ovh + 1], grow=1)
    in_h, cp_h, dpu_h = fit(body, [in_h, cp_h, dpu_h], [ov + 1, ov + 1, ov + 1], grow=1)

    root = Layout()
    root.split_column(
        Layout(Panel(Text(f"DPF / DPU LIFECYCLE   {time.strftime('%Y-%m-%d %H:%M:%S %Z')}   "
                          f"up {host_uptime()}   refresh {REFRESH}s   ns={NS}",
                          style="bold cyan"), border_style="cyan"), name="head", size=3),
        Layout(banner_panel(brows, banner_h - ov, term_w - 4), name="banner", size=banner_h),
        Layout(name="body"),
        Layout(cluster_panel(crows, cl_h - ovh), name="cl", size=cl_h),
    )
    root["body"].split_row(Layout(name="left", ratio=3), Layout(name="right", ratio=2))
    root["left"].split_column(
        Layout(operator_panel(oprows, op_h - ov, left_inner), name="op", size=op_h),
        Layout(pods_panel(comprows, comp_h - ovh, f"COMPONENTS host {NS}", "comp"),
               name="comp", size=comp_h),
        Layout(pods_panel(hcprows, hcp_h - ovh, f"HCP PODS {DPUCLUSTER}", "hcp"),
               name="hcp", size=hcp_h),
    )
    root["right"].split_column(
        Layout(inputs_panel(irows, in_h - ov, right_inner), name="in", size=in_h),
        Layout(controlplane_panel(cprows, cp_h - ov, right_inner), name="cp", size=cp_h),
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
