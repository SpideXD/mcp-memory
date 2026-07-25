"""
Cognee Contradiction Test — gpt-oss-120b + semantic evaluation
Saves every recall output to bench-results/contradiction-{backend}.log
Tests both Rust and Python Cognee.
"""
import httpx, json, time, subprocess, os, sys, shutil

OPENROUTER_KEY = "REMOVED_API_KEY"
LLM_MODEL = "openai/gpt-oss-120b"
LLM_ENDPOINT = "https://openrouter.ai/api/v1"

def run_contradiction(backend, base_url, data_dir_base):
    """Run contradiction test and log every recall response."""
    logfile = f"bench-results/contradiction-{backend}.log"
    os.makedirs("bench-results", exist_ok=True)
    
    client = httpx.Client(base_url=base_url, timeout=300)
    
    # 22 facts, 3 people, 6-year interleaved timeline
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
    
    probes = [
        ("Where does Alice work now?", "sentinela", "Alice's current job"),
        ("What does Bob do now?", "payflow", "Bob's current job"),
        ("Where does Carol work now?", "anthropic", "Carol's current job"),
        ("Did Alice ever work at Google?", "yes", "Alice Google history"),
        ("Did Bob ever work at Stripe?", "yes", "Bob Stripe history"),
        ("Did Carol ever work at DeepMind?", "yes", "Carol DeepMind history"),
        ("Where did Alice work in 2021?", "google", "Alice in 2021"),
        ("What did Bob do before starting PayFlow?", "square", "Bob pre-PayFlow"),
        ("What did Carol do at OpenAI?", "gpt", "Carol at OpenAI"),
        ("Who started their own company?", "alice", "founder check"),
        ("Who worked at Google?", "alice", "Google check"),
        ("Who raised venture capital in 2025?", "alice", "VC check"),
        ("What did Alice do after leaving Meta?", "sentinela", "Alice post-Meta"),
        ("What happened to Carol after OpenAI?", "anthropic", "Carol post-OpenAI"),
        ("Did Bob work at Square before or after Stripe?", "after", "Bob temporal order"),
    ]
    
    with open(logfile, "w") as log:
        log.write(f"=== COGNEE {backend.upper()} CONTRADICTION TEST ===\n")
        log.write(f"LLM: {LLM_MODEL}\n")
        log.write(f"Start: {time.strftime('%Y-%m-%d %H:%M:%S')}\n\n")
        
        # Phase 1: Retain all 22 facts
        t0 = time.time()
        log.write("=== RETAIN PHASE ===\n")
        for person, fact in timeline:
            start = time.time()
            dataset = f"contra_{backend}"
            r = client.post("/api/v1/remember", data={"datasetName": dataset}, 
                           files={"data": ("data.txt", fact, "text/plain")})
            dt = time.time() - start
            status = r.json().get("status", "?")
            log.write(f"  [{dt:.1f}s] {status:12s} {person}: {fact[:70]}...\n")
            print(f"  [{dt:.1f}s] {person}: {fact[:60]}...")
        
        retain_total = time.time() - t0
        log.write(f"\nRetain total: {retain_total:.0f}s (avg {retain_total/len(timeline):.1f}s)\n\n")
        
        # Phase 2: Run all 15 probes and save full responses
        log.write("=== RECALL PROBES ===\n")
        print(f"\nProbes ({len(probes)} total)...")
        
        results = []
        for i, (query, expected_keyword, label) in enumerate(probes):
            r = client.post("/api/v1/recall", json={
                "query": query,
                "datasets": [dataset],
                "topK": 3
            })
            body = r.json()
            full_text = json.dumps(body)
            
            # Extract first answer text
            answer = "NO ANSWER"
            if isinstance(body, list) and body:
                answer = body[0].get("text", str(body[0]))
            
            log.write(f"\n--- Probe {i+1}: {query} ---\n")
            log.write(f"Expected keyword: {expected_keyword}\n")
            log.write(f"Label: {label}\n")
            log.write(f"Full response:\n{json.dumps(body, indent=2)}\n")
            
            # Simple keyword check (will be manually reviewed)
            keyword_hit = expected_keyword.lower() in full_text.lower()
            status = "KEYWORD_OK" if keyword_hit else "KEYWORD_MISS"
            log.write(f"Keyword check: {status}\n")
            
            results.append({
                "query": query,
                "expected": expected_keyword,
                "keyword_hit": keyword_hit,
                "answer": answer[:200]
            })
            print(f"  {status}: {query} → {answer[:80]}...")
        
        # Summary
        hits = sum(1 for r in results if r["keyword_hit"])
        log.write(f"\n\n=== SUMMARY ===\n")
        log.write(f"Keyword matches: {hits}/{len(probes)}\n")
        log.write(f"Retain total: {retain_total:.0f}s\n")
        for i, r in enumerate(results):
            status = "✅" if r["keyword_hit"] else "❌"
            log.write(f"  {status} [{r['expected']:12s}] {r['query'][:50]}...\n")
            log.write(f"     → {r['answer'][:150]}\n")
    
    print(f"\nResults saved: {logfile}")
    return {"keyword_matches": f"{hits}/{len(probes)}", "retain_total_s": round(retain_total), "log": logfile}

# ═══════════════════════════════════════════════
# RUST COGNEE
# ═══════════════════════════════════════════════
def run_rust():
    print("=" * 60)
    print("RUST COGNEE — gpt-oss-120b Contradiction Test")
    print("=" * 60)
    
    subprocess.run(["pkill", "-f", "cognee-http-server"], capture_output=True)
    time.sleep(1)
    shutil.rmtree("/tmp/cognee-test-rust", ignore_errors=True)
    
    env = os.environ.copy()
    env.update({
        "LLM_PROVIDER": "openai", "LLM_API_KEY": OPENROUTER_KEY,
        "LLM_MODEL": LLM_MODEL, "LLM_ENDPOINT": LLM_ENDPOINT,
        "EMBEDDING_ENDPOINT": "http://localhost:8080/v1",
        "EMBEDDING_PROVIDER": "openai_compatible",
        "EMBEDDING_API_KEY": "not-needed", "EMBEDDING_DIMENSIONS": "1024",
        "VECTOR_DB_PROVIDER": "lancedb", "GRAPH_DB_PROVIDER": "ladybug",
        "ENABLE_BACKEND_ACCESS_CONTROL": "false",
        "DATA_ROOT_DIRECTORY": "/tmp/cognee-test-rust/data",
        "SYSTEM_ROOT_DIRECTORY": "/tmp/cognee-test-rust/system",
    })
    
    proc = subprocess.Popen(
        ["./cognee-rs/target/release/cognee-http-server", "--port", "9876"],
        env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
    )
    
    # Wait for health
    for i in range(30):
        try:
            if httpx.get("http://localhost:9876/health", timeout=3).status_code == 200:
                print(f"Rust ready in {i+1}s")
                break
        except: pass
        time.sleep(1)
    
    result = run_contradiction("rust", "http://localhost:9876", "/tmp/cognee-test-rust")
    proc.terminate()
    proc.wait(timeout=10)
    return result

# ═══════════════════════════════════════════════
# PYTHON COGNEE
# ═══════════════════════════════════════════════
def run_python():
    print("\n" + "=" * 60)
    print("PYTHON COGNEE — gpt-oss-120b Contradiction Test")
    print("=" * 60)
    
    subprocess.run(["pkill", "-f", "uvicorn.*cognee"], capture_output=True)
    time.sleep(1)
    shutil.rmtree("/tmp/cognee-test-py", ignore_errors=True)
    
    env = os.environ.copy()
    env.update({
        "STRUCTURED_OUTPUT_FRAMEWORK": "instructor",
        "LLM_INSTRUCTOR_MODE": "json_mode",  # needed for DeepSeek, still safe with gpt-oss
        "LLM_API_KEY": OPENROUTER_KEY, "LLM_MODEL": LLM_MODEL,
        "LLM_ENDPOINT": LLM_ENDPOINT, "LLM_PROVIDER": "openai",
        "EMBEDDING_ENDPOINT": "http://localhost:8080/v1",
        "EMBEDDING_PROVIDER": "llama_cpp",
        "EMBEDDING_API_KEY": "not-needed", "EMBEDDING_DIMENSIONS": "1024",
        "COGNEE_SKIP_CONNECTION_TEST": "true",
        "VECTOR_DB_PROVIDER": "lancedb", "GRAPH_DB_PROVIDER": "ladybug",
        "ENABLE_BACKEND_ACCESS_CONTROL": "false",
        "COGNEE_DATA_DIR": "/tmp/cognee-test-py",
    })
    
    proc = subprocess.Popen(
        ["./cognee-venv/bin/python", "-m", "uvicorn", "cognee.api.client:app",
         "--host", "0.0.0.0", "--port", "8000"],
        env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
    )
    
    for i in range(60):
        try:
            if httpx.get("http://localhost:8000/health", timeout=3).status_code == 200:
                print(f"Python ready in {i+1}s")
                break
        except: pass
        time.sleep(1)
    
    result = run_contradiction("python", "http://localhost:8000", "/tmp/cognee-test-py")
    proc.terminate()
    proc.wait(timeout=10)
    return result

# ═══════════════════════════════════════════════
if __name__ == "__main__":
    backend = sys.argv[1] if len(sys.argv) > 1 else "both"
    
    if backend == "rust":
        run_rust()
    elif backend == "python":
        run_python()
    else:
        rust_result = run_rust()
        python_result = run_python()
        print(f"\nRust: {rust_result['keyword_matches']} keyword, {rust_result['retain_total_s']}s retain")
        print(f"Python: {python_result['keyword_matches']} keyword, {python_result['retain_total_s']}s retain")
        print(f"\nLogs: bench-results/contradiction-rust.log")
        print(f"       bench-results/contradiction-python.log")
