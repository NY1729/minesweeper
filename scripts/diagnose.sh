#!/usr/bin/env bash
# Collect what is needed to find out why the server is slow or the backend is down.
# Read-only and cheap; every command has a timeout so one stuck command can't hang the rest.
# Run it from your PC over ONE ssh connection (nothing needs to be installed on the server):
#   ssh yuu@<server> 'bash -s' < scripts/diagnose.sh | tee diag.txt
export LC_ALL=C
sec() { printf '\n===== %s =====\n' "$1"; }
run() { timeout "${T:-10}" "$@" 2>&1 || echo "(failed or timed out: $*)"; }

sec "time, uptime, load average (compare with the CPU count)"
date; uptime; echo "CPUs: $(nproc)"

sec "memory and swap, MB (swap in heavy use = the machine is thrashing)"
free -m

sec "pressure stall info (avg10 > 10 on 'some' = processes are waiting for that resource)"
for f in cpu memory io; do printf '%s: ' $f; head -1 /proc/pressure/$f 2>/dev/null || echo n/a; done

sec "vmstat 3 samples (r = runnable, b = blocked, si/so = swap in/out, wa = waiting for disk, st = stolen by the host)"
run vmstat 1 3

sec "top 12 processes by CPU"
ps -eo pid,stat,pcpu,pmem,rss,etime,args --sort=-pcpu | head -13 | cut -c1-170

sec "top 8 processes by memory (rss in KB)"
ps -eo pid,stat,pcpu,pmem,rss,etime,args --sort=-rss | head -9 | cut -c1-170

sec "processes stuck on disk (D) or zombies (Z)"
ps -eo stat,pid,args | awk '$1 ~ /^[DZ]/' | head -10 | cut -c1-150

sec "leftovers from tests or builds that could still be running"
ps -eo pid,etime,pcpu,args | grep -E 'loadtest|go (run|build)|docker build|buildkit|npm|vite' | grep -v grep | cut -c1-170

sec "disk space and inodes"
df -h / /var/lib/docker 2>/dev/null; df -i / | tail -2

sec "kernel messages: OOM kills, hung tasks, conntrack, disk errors (needs sudo; skipped if it asks for a password)"
( sudo -n dmesg -T 2>/dev/null || dmesg -T 2>/dev/null ) | grep -iE 'out of memory|oom-kill|killed process|blocked for more than|hung task|nf_conntrack|table full|I/O error' | tail -15
echo "(end of kernel messages)"

sec "connection table (a full table drops new connections, which looks like 'ssh is slow')"
echo "conntrack: $(cat /proc/sys/net/netfilter/nf_conntrack_count 2>/dev/null || echo n/a) / $(cat /proc/sys/net/netfilter/nf_conntrack_max 2>/dev/null || echo n/a)"
echo "TCP connections by state:"; ss -tan 2>/dev/null | awk 'NR>1{c[$1]++} END{for(k in c) print "  " c[k], k}' | sort -rn
echo "listening sockets (Recv-Q close to Send-Q = the accept queue is full):"; ss -ltn 2>/dev/null | head -12

sec "who is connected: top remote IPs by number of connections"
ss -tn 2>/dev/null | awk 'NR>1{n=split($5,a,":"); ip=$5; sub(":[0-9]+$","",ip); c[ip]++} END{for(k in c) print c[k], k}' | sort -rn | head -8

sec "ssh login attempts in the last hour (a flood of 'Failed' = someone is hammering the server)"
( sudo -n journalctl -u ssh -u sshd --since '1 hour ago' --no-pager 2>/dev/null || journalctl -u ssh -u sshd --since '1 hour ago' --no-pager 2>/dev/null ) | grep -ciE 'failed|invalid user' | sed 's/^/failed or invalid attempts: /'
w -h 2>/dev/null | head -5

sec "docker containers"
T=15 run docker ps -a --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'
sec "docker resource use right now"
T=20 run docker stats --no-stream --format 'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.PIDs}}'
sec "docker disk use"
T=20 run docker system df

sec "compose: backend and cloudflared logs"
cd "$HOME/minesweeper" 2>/dev/null && {
  T=15 run docker compose ps -a
  echo "--- backend, last 40 lines"; T=15 run docker compose logs --no-color --tail 40 backend
  echo "--- cloudflared, last 15 lines"; T=15 run docker compose logs --no-color --tail 15 cloudflared
}
echo; echo "===== done: $(date) ====="
