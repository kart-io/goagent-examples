#!/usr/bin/env python3
"""Exercise the compiled synthetic flight CLI; never enables paid/live models."""
import argparse
import json
from pathlib import Path
import subprocess
import tempfile


def verify(binary):
    root = Path(__file__).resolve().parents[1]
    fixture = root / "orchestration/flight-booking/fixtures/one-way.json"
    family = root / "orchestration/flight-booking/fixtures/round-trip-family.json"
    results = []

    def case(name, check):
        with tempfile.TemporaryDirectory(prefix="flight-smoke-") as state:
            def call(action, *args, code=0):
                cmd = [str(binary), "-dir", state, "-at", "2029-01-01T00:00:00Z",
                       "-action", action, *map(str, args)]
                p = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
                assert p.returncode == code, (name, action, p.returncode, p.stderr)
                return json.loads(p.stdout) if code == 0 else None
            check(call)
        results.append({"case": name, "passed": True})

    def start(call, scenario="normal", request_file=fixture):
        r = call("start", "-input", request_file, "-scenario", scenario)
        assert r["status"] == "awaiting_selection", r["status"]
        assert r["effects"]["payments"] == 0
        return r

    def select(call, first):
        r = call("select", "-quote", first["offers"][0]["id"],
                 "-token", first["confirmation"]["token"])
        assert r["status"] == "awaiting_final_confirmation"
        assert r["effects"]["payments"] == 0
        assert r["confirmation"]["token"] != first["confirmation"]["token"]
        return r

    def final(call, r, *extra, code=0):
        return call("confirm", "-token", r["confirmation"]["token"], *extra, code=code)

    def normal(call):
        r = start(call)
        comparison = call("compare")
        assert comparison["read_only_tools_this_invocation"] == 1
        assert comparison["model_calls_this_invocation"] == 2
        r = final(call, select(call, r))
        assert r["status"] == "ticketed" and r["ticket"]
        assert r["effects"]["payments"] == r["effects"]["tickets"] == 1
        assert call("status")["ticket"] == r["ticket"]
    case("structured-and-readonly-agent", normal)

    def chat(call):
        assert call("message", "-message", "我要订机票")["status"] == "needs_input"
        r = call("message", "-message", "2030年1月10日从新加坡SIN到上海PVG，单程经济舱，一位成人，预算5000元")
        assert r["status"] == "awaiting_selection" and r["task_usage"]["total_tokens"] > 0
    case("agent-clarification", chat)

    def family_case(call):
        r = start(call, request_file=family)
        assert len(r["offers"][0]["legs"]) == 2
        assert len(r["offers"][0]["travelers"]) == 2
        assert final(call, select(call, r))["status"] == "ticketed"
    case("roundtrip-family", family_case)

    def missing(call):
        r = call("start", "-input", root / "orchestration/flight-booking/fixtures/missing-fields.json")
        assert r["status"] == "needs_input" and r["effects"]["payments"] == 0
    case("missing-fields", missing)

    def no_flights(call):
        r = call("start", "-input", fixture, "-scenario", "no-flights")
        assert r["status"] == "rejected" and r["effects"]["payments"] == 0
    case("no-flights", no_flights)

    for name, params in [("price-change", ["-price-delta", "100"]),
                         ("currency-change", ["-currency", "USD"]),
                         ("rules-change", ["-rules", "changed"]),
                         ("sold-out", ["-sold-out"]),
                         ("expired", ["-at", "2029-01-02T00:00:00Z"])]:
        def invalidate(call, params=params):
            r = final(call, select(call, start(call)), *params)
            assert r["status"] == "requote_required" and r["effects"]["payments"] == 0
        case(name, invalidate)

    for scenario, expected in [("payment-declined", "rejected"),
                               ("payment-unknown", "ticketed"),
                               ("ticket-unknown", "ticketed"),
                               ("ticket-failed", "refunded"),
                               ("refund-unknown", "refunded")]:
        def failure(call, scenario=scenario, expected=expected):
            r = final(call, select(call, start(call, scenario)))
            for _ in range(3):
                if r["status"] not in ("payment_pending", "ticketing_pending", "refund_pending"):
                    break
                r = call("status")
            assert r["status"] == expected, r["status"]
            assert r["effects"]["payments"] <= 1 and r["effects"]["tickets"] <= 1
            assert call("status")["effects"] == r["effects"]
        case(scenario, failure)

    def invalid(call):
        r = select(call, start(call))
        call("confirm", "-token", "wrong", code=1)
        final(call, r, "-actor", "intruder", code=1)
        final(call, r, "-input", family, code=1)
        assert call("status")["effects"]["payments"] == 0
    case("reject-invalid-authorizations", invalid)

    def revise(call):
        old = select(call, start(call))
        r = call("revise", "-input", family)
        assert r["revision"] == 2 and r["status"] == "awaiting_selection"
        final(call, old, code=1)
        r = call("cancel", "-revision", "2")
        assert r["status"] == "canceled" and r["effects"]["payments"] == 0
    case("revision-and-cancel", revise)

    for stage in ("payment", "ticket", "refund"):
        def crash(call, stage=stage):
            scenario = "ticket-failed" if stage == "refund" else "normal"
            r = select(call, start(call, scenario))
            final(call, r, "-crash-after", stage, code=86)
            done = call("status")
            assert done["status"] == ("refunded" if stage == "refund" else "ticketed")
            assert done["effects"]["payments"] == 1
            assert call("status")["effects"] == done["effects"]
        case("crash-after-" + stage, crash)
    return {"simulated": True, "live_model": False, "case_count": len(results), "cases": results}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    report = verify(args.binary.resolve())
    data = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    if args.output:
        args.output.write_text(data)
    print(data, end="")


if __name__ == "__main__":
    main()
