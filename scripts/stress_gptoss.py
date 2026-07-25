"""
Unified Cognee Stress Test — gpt-oss-120b
Runs same contradiction + scale + concurrent tests against Rust and Python Cognee.
"""
import httpx, json, time, subprocess, os, sys, concurrent.futures, psutil, shutil

OPENROUTER_KEY = "REMOVED_API_KEY"
LLM_MODEL = "openai/gpt-oss-120b"
LLM_ENDPOINT = "https://openrouter.ai/api/v1"
EMBEDDING_ENDPOINT = "http://localhost:8080/v1"
EMBEDDING_DIM = "1024"

def wait_healthy(url, timeout=120):
    """Wait for Cognee server to become healthy."""
    start = time.time()
    while time.time() - start < timeout:
        try:
            r = httpx.get(f"{url}/health", timeout=5)
            if r.status_code == 200:
                return time.time() - start
        except:
            pass
        time.sleep(2)
    raise TimeoutError(f"Server at {url} did not become healthy within {timeout}s")

def run_stress(base_url, label):
    """Run the full stress suite against a Cognee HTTP server."""
    client = httpx.Client(base_url=base_url, timeout=300)
    results = {}
    
    def remember(dataset, content):
        return client.post("/api/v1/remember", data={
            "datasetName": dataset,
        }, files={
            "data": ("data.txt", content, "text/plain")
        })
    
    def recall(query, dataset, top_k=3):
        return client.post("/api/v1/recall", json={
            "query": query, "datasets": [dataset], "topK": top_k
        })
    
    def check(query, dataset, expected_contains, label=""):
        r = recall(query, dataset)
        try:
            body = json.dumps(r.json()).lower()
        except:
            body = r.text.lower()
        hit = expected_contains.lower() in body
        marker = "PASS" if hit else "FAIL"
        print(f"    {marker}: '{query}' → expected '{expected_contains}'")
        return hit
    
    # Memory snapshot — find the right process
    try:
        pid = int(subprocess.check_output(["pgrep", "-f", "cognee-http-server"]).decode().strip().split("\n")[0])
    except:
        pid = int(subprocess.check_output(["pgrep", "-f", "uvicorn.*cognee"]).decode().strip().split("\n")[0])
    mem_start = psutil.Process(pid).memory_info().rss // 1024
    cpu_start = psutil.Process(pid).cpu_percent(interval=0.5)
    results["startup"] = {"mem_start_kb": mem_start, "cpu_start_pct": cpu_start}
    
    T0 = time.time()
    
    # ═══════════════════════════════════════════
    # TEST 1: HARD CONTRADICTION — 22 facts, 3 people
    # ═══════════════════════════════════════════
    print(f"\n{'='*60}")
    print(f"TEST 1: HARD CONTRADICTION ({label})")
    print(f"{'='*60}")
    
    timeline = [
        ("Alice", "Alice graduated from MIT with a CS degree in 2018."),
        ("Bob", "Bob graduated from Stanford with an MBA in 2018."),
        ("Carol", "Carol finished her PhD in AI at Berkeley in 2018."),
        ("Alice", "Alice joined Google as a junior software engineer in 2019."),
        ("Bob", "Bob started as a product manager at Stripe in 2019."),
        ("Carol", "Carol joined DeepMind as a research scientist in 2019."),
        ("Alice", "Alice was promoted to software engineer at Google in 2020."),
        ("Bob", "Bob moved from Stripe to Square as a senior PM in 2020."),
        ("Carol", "Carol published her breakthrough paper on transformer architectures in 2020."),
        ("Alice", "Alice became a senior engineer at Google in 2021."),
        ("Bob", "Bob was promoted to director of product at Square in 2021."),
        ("Carol", "Carol left DeepMind to join OpenAI as a research lead in 2021."),
        ("Alice", "Alice left Google and joined Meta as a staff engineer in 2022."),
        ("Bob", "Bob left Square to start his own fintech company PayFlow in 2022."),
        ("Carol", "Carol led the GPT-5 safety team at OpenAI starting in 2022."),
        ("Alice", "Alice was promoted to senior staff engineer at Meta in 2023."),
        ("Bob", "Bob raised $10M Series A for PayFlow in 2023."),
        ("Carol", "Carol was promoted to director of research at OpenAI in 2023."),
        ("Alice", "Alice left Meta to start her own AI security startup Sentinela in 2024."),
        ("Bob", "Bob's PayFlow reached 100,000 users in 2024."),
        ("Carol", "Carol left OpenAI and joined Anthropic as VP of Research in 2024."),
        ("Alice", "Alice's startup Sentinela raised $25M Series A in 2025."),
    ]
    
    t1 = time.time()
    retain_times = []
    for person, fact in timeline:
        start = time.time()
        remember(f"contra_{label}", fact)
        dt = time.time() - start
        retain_times.append(dt)
        print(f"  [{dt:.1f}s] {person}: {fact[:60]}...")
    
    retain_total = time.time() - t1
    print(f"  Retain total: {retain_total:.1f}s (avg {retain_total/len(timeline):.1f}s)")
    
    probes = [
        ("Where does Alice work now?", f"contra_{label}", "sentinela"),
        ("What does Bob do now?", f"contra_{label}", "payflow"),
        ("Where does Carol work now?", f"contra_{label}", "anthropic"),
        ("Did Alice ever work at Google?", f"contra_{label}", "yes"),
        ("Did Bob ever work at Stripe?", f"contra_{label}", "yes"),
        ("Did Carol ever work at DeepMind?", f"contra_{label}", "yes"),
        ("Where did Alice work in 2021?", f"contra_{label}", "google"),
        ("What did Bob do before starting PayFlow?", f"contra_{label}", "square"),
        ("What did Carol do at OpenAI?", f"contra_{label}", "gpt"),
        ("Who started their own company?", f"contra_{label}", "alice"),
        ("Who worked at Google?", f"contra_{label}", "alice"),
        ("Who raised venture capital in 2025?", f"contra_{label}", "alice"),
        ("What did Alice do after leaving Meta?", f"contra_{label}", "sentinela"),
        ("What happened to Carol after OpenAI?", f"contra_{label}", "anthropic"),
        ("Did Bob work at Square before or after Stripe?", f"contra_{label}", "after"),
    ]
    
    matches = 0
    for q, ds, expected in probes:
        if check(q, ds, expected, q[:50]):
            matches += 1
    
    results["contradiction"] = {
        "score": f"{matches}/{len(probes)}",
        "pct": round(matches * 100 / len(probes)),
        "retain_total_s": round(retain_total, 1),
        "avg_retain_s": round(retain_total / len(timeline), 1),
        "facts": len(timeline),
    }
    
    # ═══════════════════════════════════════════
    # TEST 2: SCALE 30
    # ═══════════════════════════════════════════
    print(f"\n{'='*60}")
    print(f"TEST 2: SCALE 30 ({label})")
    print(f"{'='*60}")
    
    facts = [f"Person {i} lives in City {i} and works as Job {i} at Company {i}." for i in range(1, 31)]
    
    t1 = time.time()
    for i, fact in enumerate(facts):
        start = time.time()
        remember(f"scale_{label}", fact)
        if (i+1) % 10 == 0:
            print(f"  Retain {i+1}/30 ({time.time()-start:.1f}s)")
    
    retain_scale = time.time() - t1
    print(f"  Total: {retain_scale:.1f}s (avg {retain_scale/30:.1f}s)")
    
    matches = 0
    for i in [1, 5, 10, 15, 20, 25, 30]:
        if check(f"Where does Person {i} live?", f"scale_{label}", f"city {i}", f"P{i}"):
            matches += 1
    
    results["scale30"] = {
        "score": f"{matches}/7",
        "pct": round(matches * 100 / 7),
        "retain_total_s": round(retain_scale, 1),
        "avg_retain_s": round(retain_scale / 30, 1),
    }
    
    # ═══════════════════════════════════════════
    # TEST 3: CONCURRENT
    # ═══════════════════════════════════════════
    print(f"\n{'='*60}")
    print(f"TEST 3: CONCURRENT WRITES ({label})")
    print(f"{'='*60}")
    
    t1 = time.time()
    with concurrent.futures.ThreadPoolExecutor(max_workers=5) as ex:
        futures = [ex.submit(remember, f"concur_{label}", f"Concurrent fact {i}: Item {i} unique data.")
                   for i in range(1, 6)]
        for f in futures:
            f.result()
    wall = time.time() - t1
    print(f"  Wall time: {wall:.1f}s")
    
    time.sleep(30)  # pipeline drain
    matches = 0
    for i in range(1, 6):
        if check(f"Concurrent fact {i}", f"concur_{label}", f"item {i}", f"Fact {i}"):
            matches += 1
    
    results["concurrent"] = {
        "score": f"{matches}/5",
        "wall_s": round(wall, 1),
    }
    
    # ═══════════════════════════════════════════
    # METRICS
    # ═══════════════════════════════════════════
    mem_end = psutil.Process(pid).memory_info().rss // 1024
    cpu_end = psutil.Process(pid).cpu_percent(interval=1)
    total_wall = time.time() - T0
    
    results["metrics"] = {
        "wall_total_s": round(total_wall, 1),
        "mem_start_kb": mem_start,
        "mem_end_kb": mem_end,
        "mem_delta_kb": mem_end - mem_start,
        "cpu_start_pct": cpu_start,
        "cpu_end_pct": cpu_end,
    }
    
    print(f"\n{'='*60}")
    print(f"RESULTS: {label}")
    print(f"{'='*60}")
    print(json.dumps(results, indent=2))
    return results

# ═══════════════════════════════════════════
# MAIN: Run both backends
# ═══════════════════════════════════════════
if __name__ == "__main__":
    os.makedirs("bench-results", exist_ok=True)
    ts = time.strftime("%Y%m%d-%H%M%S")
    all_results = {}
    
    backend = sys.argv[1] if len(sys.argv) > 1 else "rust"
    
    if backend == "rust":
        print("=" * 60)
        print("COGNEE RUST — gpt-oss-120b STRESS TEST")
        print("=" * 60)
        
        # Kill existing
        subprocess.run(["pkill", "-f", "cognee-http-server"], capture_output=True)
        time.sleep(2)
        
        # Start Rust Cognee
        env = os.environ.copy()
        env.update({
            "LLM_PROVIDER": "openai",
            "LLM_API_KEY": OPENROUTER_KEY,
            "LLM_MODEL": LLM_MODEL,
            "LLM_ENDPOINT": LLM_ENDPOINT,
            "EMBEDDING_ENDPOINT": EMBEDDING_ENDPOINT,
            "EMBEDDING_PROVIDER": "openai_compatible",
            "EMBEDDING_API_KEY": "not-needed",
            "EMBEDDING_DIMENSIONS": EMBEDDING_DIM,
            "EMBEDDING_MODEL_NAME": "qwen3-embedding-0.6b",
            "VECTOR_DB_PROVIDER": "lancedb",
            "GRAPH_DB_PROVIDER": "ladybug",
            "ENABLE_BACKEND_ACCESS_CONTROL": "false",
            "DATA_ROOT_DIRECTORY": "/tmp/cognee-bench-rust-gptoss/data",
            "SYSTEM_ROOT_DIRECTORY": "/tmp/cognee-bench-rust-gptoss/system",
        })
        
        # Clean data
        shutil.rmtree("/tmp/cognee-bench-rust-gptoss", ignore_errors=True)
        
        print("Starting Rust Cognee...")
        start_start = time.time()
        proc = subprocess.Popen(
            ["./cognee-rs/target/release/cognee-http-server", "--port", "9876"],
            env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
        )
        
        startup_s = wait_healthy("http://localhost:9876")
        print(f"Rust startup: {startup_s:.1f}s")
        all_results["rust_startup_s"] = round(startup_s, 1)
        
        all_results["rust"] = run_stress("http://localhost:9876", "rust_gptoss")
        
        proc.terminate()
        proc.wait(timeout=10)
    
    elif backend == "python":
        print("=" * 60)
        print("COGNEE PYTHON — gpt-oss-120b STRESS TEST")
        print("=" * 60)
        
        # Kill existing Python
        subprocess.run(["pkill", "-f", "uvicorn.*cognee"], capture_output=True)
        subprocess.run(["pkill", "-f", "cognee-python"], capture_output=True)
        time.sleep(2)
        
        # Clean data
        shutil.rmtree("/tmp/cognee-bench-py-gptoss", ignore_errors=True)
        
        env = os.environ.copy()
        env.update({
            "STRUCTURED_OUTPUT_FRAMEWORK": "instructor",
            "LLM_INSTRUCTOR_MODE": "json_mode",
            "LLM_API_KEY": OPENROUTER_KEY,
            "LLM_MODEL": LLM_MODEL,
            "LLM_ENDPOINT": LLM_ENDPOINT,
            "LLM_PROVIDER": "openai",
            "EMBEDDING_ENDPOINT": EMBEDDING_ENDPOINT,
            "EMBEDDING_PROVIDER": "llama_cpp",
            "EMBEDDING_API_KEY": "not-needed",
            "EMBEDDING_DIMENSIONS": EMBEDDING_DIM,
            "COGNEE_SKIP_CONNECTION_TEST": "true",
            "VECTOR_DB_PROVIDER": "lancedb",
            "GRAPH_DB_PROVIDER": "ladybug",
            "ENABLE_BACKEND_ACCESS_CONTROL": "false",
            "COGNEE_DATA_DIR": "/tmp/cognee-bench-py-gptoss",
        })
        
        print("Starting Python Cognee...")
        start_start = time.time()
        proc = subprocess.Popen(
            ["./cognee-venv/bin/python", "-m", "uvicorn", "cognee.api.client:app",
             "--host", "0.0.0.0", "--port", "8000"],
            env=env,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
        )
        
        startup_s = wait_healthy("http://localhost:8000")
        print(f"Python startup: {startup_s:.1f}s")
        all_results["python_startup_s"] = round(startup_s, 1)
        
        all_results["python"] = run_stress("http://localhost:8000", "py_gptoss")
        
        proc.terminate()
        proc.wait(timeout=10)
    
    elif backend == "both":
        # Run both sequentially via self-invocation
        print("Running Rust first...")
        subprocess.run([sys.executable, __file__, "rust"], check=True)
        print("\nRunning Python...")
        subprocess.run([sys.executable, __file__, "python"], check=True)
        
        # Load and compare
        import glob
        files = sorted(glob.glob("bench-results/gptoss-*.json"))
        if len(files) >= 2:
            with open(files[-2]) as f: rust_data = json.load(f)
            with open(files[-1]) as f: py_data = json.load(f)
            
            # Print comparison
            print("\n" + "=" * 70)
            print("COMPARISON: Rust vs Python Cognee (gpt-oss-120b)")
            print("=" * 70)
            
            print(f"\n{'Metric':<35} {'Rust':>15} {'Python':>15}")
            print("-" * 65)
            
            for key in ["contradiction", "scale30", "concurrent"]:
                r = rust_data.get(key, {})
                p = py_data.get(key, {})
                if r and p:
                    print(f"{key} accuracy:")
                    print(f"  {'  score':<33} {r.get('score','?'):>15} {p.get('score','?'):>15}")
                    print(f"  {'  avg retain':<33} {r.get('avg_retain_s','?'):>14.1f}s {p.get('avg_retain_s','?'):>14.1f}s")
            
            rm = rust_data.get("metrics", {})
            pm = py_data.get("metrics", {})
            print(f"\n{'Memory start (KB)':<35} {rm.get('mem_start_kb','?'):>15} {pm.get('mem_start_kb','?'):>15}")
            print(f"{'Memory end (KB)':<35} {rm.get('mem_end_kb','?'):>15} {pm.get('mem_end_kb','?'):>15}")
            print(f"{'Memory delta (KB)':<35} {rm.get('mem_delta_kb','?'):>15} {pm.get('mem_delta_kb','?'):>15}")
            print(f"{'Total wall (s)':<35} {rm.get('wall_total_s','?'):>14.1f} {pm.get('wall_total_s','?'):>14.1f}")
            print(f"{'Startup time (s)':<35} {rust_data.get('rust_startup_s','?'):>14.1f} {py_data.get('python_startup_s','?'):>14.1f}")
        
        sys.exit(0)
    
    # Save results
    outfile = f"bench-results/gptoss-{backend}-{ts}.json"
    with open(outfile, "w") as f:
        json.dump(all_results, f, indent=2)
    print(f"\nSaved: {outfile}")
