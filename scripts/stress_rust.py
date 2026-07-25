"""
Rust Cognee Full Stress Test: Scale 50, Concurrent, Hard Contradiction
"""
import httpx, json, time, subprocess, os, concurrent.futures, psutil

BASE = "http://localhost:9876"
client = httpx.Client(base_url=BASE, timeout=300)
results = {}

def remember(dataset, content):
    return client.post("/api/v1/remember", files={
        "datasetName": (None, dataset),
        "data": ("data.txt", content)
    })

def recall(query, dataset, top_k=3):
    return client.post("/api/v1/recall", json={
        "query": query, "datasets": [dataset], "topK": top_k
    })

def check(query, dataset, expected_contains, label=""):
    r = recall(query, dataset)
    body = json.dumps(r.json()).lower()
    hit = expected_contains.lower() in body
    marker = "✅" if hit else "❌"
    print(f"  {marker} {label}: '{query}' → expected '{expected_contains}'")
    return hit

T0 = time.time()
pid = int(subprocess.check_output(["pgrep", "-f", "cognee-http-server"]).decode().strip().split("\n")[0])
mem_start = psutil.Process(pid).memory_info().rss // 1024

# ═══════════════════════════════════════════════════════════
# TEST 1: HARD CONTRADICTION (3 people, 22 facts, 6 years)
# ═══════════════════════════════════════════════════════════
print("=" * 60)
print("TEST 1: HARD CONTRADICTION — 3 People, 22 Facts")
print("=" * 60)

# Interleaved timeline — test entity disambiguation
timeline = [
    # 2018
    ("Alice", "Alice graduated from MIT with a CS degree in 2018."),
    ("Bob", "Bob graduated from Stanford with an MBA in 2018."),
    ("Carol", "Carol finished her PhD in AI at Berkeley in 2018."),
    # 2019
    ("Alice", "Alice joined Google as a junior software engineer in 2019."),
    ("Bob", "Bob started as a product manager at Stripe in 2019."),
    ("Carol", "Carol joined DeepMind as a research scientist in 2019."),
    # 2020
    ("Alice", "Alice was promoted to software engineer at Google in 2020."),
    ("Bob", "Bob moved from Stripe to Square as a senior PM in 2020."),
    ("Carol", "Carol published her breakthrough paper on transformer architectures in 2020."),
    # 2021
    ("Alice", "Alice became a senior engineer at Google in 2021."),
    ("Bob", "Bob was promoted to director of product at Square in 2021."),
    ("Carol", "Carol left DeepMind to join OpenAI as a research lead in 2021."),
    # 2022
    ("Alice", "Alice left Google and joined Meta as a staff engineer in 2022."),
    ("Bob", "Bob left Square to start his own fintech company PayFlow in 2022."),
    ("Carol", "Carol led the GPT-5 safety team at OpenAI starting in 2022."),
    # 2023
    ("Alice", "Alice was promoted to senior staff engineer at Meta in 2023."),
    ("Bob", "Bob raised $10M Series A for PayFlow in 2023."),
    ("Carol", "Carol was promoted to director of research at OpenAI in 2023."),
    # 2024
    ("Alice", "Alice left Meta to start her own AI security startup Sentinela in 2024."),
    ("Bob", "Bob's PayFlow reached 100,000 users in 2024."),
    ("Carol", "Carol left OpenAI and joined Anthropic as VP of Research in 2024."),
    # 2025
    ("Alice", "Alice's startup Sentinela raised $25M Series A in 2025."),
]

t1 = time.time()
for person, fact in timeline:
    start = time.time()
    remember("hard_contra", fact)
    dt = time.time() - start
    print(f"  {person}: {fact[:60]}... ({dt:.0f}s)")

retain_hard = time.time() - t1

print(f"\n  Retain total: {retain_hard:.0f}s (avg {retain_hard/len(timeline):.1f}s)")
print(f"  Probing...")

probes = [
    # Current state
    ("Where does Alice work now?", "hard_contra", "sentinela"),
    ("What does Bob do now?", "hard_contra", "payflow"),
    ("Where does Carol work now?", "hard_contra", "anthropic"),
    # Historical
    ("Did Alice ever work at Google?", "hard_contra", "yes"),
    ("Did Bob ever work at Stripe?", "hard_contra", "yes"),
    ("Did Carol ever work at DeepMind?", "hard_contra", "yes"),
    # Temporal reasoning
    ("Where did Alice work in 2021?", "hard_contra", "google"),
    ("What did Bob do before starting PayFlow?", "hard_contra", "square"),
    ("What did Carol do at OpenAI?", "hard_contra", "gpt"),
    # Cross-person
    ("Who started their own company?", "hard_contra", "alice"),
    ("Who worked at Google?", "hard_contra", "alice"),
    ("Who raised venture capital in 2025?", "hard_contra", "alice"),
    # Sequence
    ("What did Alice do after leaving Meta?", "hard_contra", "sentinela"),
    ("What happened to Carol after OpenAI?", "hard_contra", "anthropic"),
    ("Did Bob work at Square before or after Stripe?", "hard_contra", "after"),
]

matches = 0
for q, ds, expected in probes:
    if check(q, ds, expected, q[:50]):
        matches += 1

results["contradiction"] = {
    "p_at_1": f"{matches}/{len(probes)}",
    "pct": round(matches * 100 / len(probes)),
    "retain_s": round(retain_hard),
    "facts": len(timeline),
    "avg_retain_s": round(retain_hard / len(timeline), 1)
}

# ═══════════════════════════════════════════════════════════
# TEST 2: SCALE 50
# ═══════════════════════════════════════════════════════════
print("\n" + "=" * 60)
print("TEST 2: SCALE 50")
print("=" * 60)

facts_50 = [f"Person {i} lives in City {i} and works as Job {i} at Company {i}." for i in range(1, 51)]

t1 = time.time()
for i, fact in enumerate(facts_50):
    start = time.time()
    remember("scale50", fact)
    dt = time.time() - start
    if (i+1) % 10 == 0:
        print(f"  Retain {i+1}/50: {dt:.0f}s (total: {time.time()-t1:.0f}s)")

retain_scale = time.time() - t1
print(f"  Total: {retain_scale:.0f}s (avg {retain_scale/50:.1f}s)")
print(f"  Probing 10 random queries...")

matches = 0
for i in [1, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50]:
    if check(f"Where does Person {i} live?", "scale50", f"city {i}", f"P{i}"):
        matches += 1

results["scale50"] = {
    "p_at_1": f"{matches}/11",
    "pct": round(matches * 100 / 11),
    "retain_s": round(retain_scale),
    "avg_retain_s": round(retain_scale / 50, 1)
}

# ═══════════════════════════════════════════════════════════
# TEST 3: CONCURRENT WRITES
# ═══════════════════════════════════════════════════════════
print("\n" + "=" * 60)
print("TEST 3: CONCURRENT WRITES (5 parallel remembers)")
print("=" * 60)

t1 = time.time()
with concurrent.futures.ThreadPoolExecutor(max_workers=5) as ex:
    futures = [ex.submit(remember, "concurrent", f"Concurrent fact {i}: Item {i} unique data.") 
               for i in range(1, 6)]
    for f in futures:
        f.result()
wall_time = time.time() - t1
print(f"  Wall time: {wall_time:.0f}s (5 concurrent retains)")
print(f"  Probing...")

time.sleep(30)  # Pipeline completion

matches = 0
for i in range(1, 6):
    if check(f"Concurrent fact {i}", "concurrent", f"item {i}", f"Fact {i}"):
        matches += 1

results["concurrent"] = {
    "facts_found": f"{matches}/5",
    "wall_s": round(wall_time),
}

# ═══════════════════════════════════════════════════════════
# SUMMARY
# ═══════════════════════════════════════════════════════════
mem_end = psutil.Process(pid).memory_info().rss // 1024
cpu = psutil.Process(pid).cpu_percent(interval=1)
total_wall = time.time() - T0

results["metrics"] = {
    "wall_s": round(total_wall),
    "mem_start_kb": mem_start,
    "mem_end_kb": mem_end,
    "mem_delta_kb": mem_end - mem_start,
    "cpu_pct": cpu,
}

print("\n" + "=" * 60)
print("RESULTS")
print("=" * 60)
print(json.dumps(results, indent=2))

os.makedirs("bench-results", exist_ok=True)
ts = time.strftime("%Y%m%d-%H%M%S")
with open(f"bench-results/rust-full-{ts}.json", "w") as f:
    json.dump(results, f, indent=2)
print(f"\nSaved: bench-results/rust-full-{ts}.json")
