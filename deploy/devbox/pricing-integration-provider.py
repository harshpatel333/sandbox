#!/usr/bin/env python3
import json
import os
from pathlib import Path
import urllib.error
import urllib.request

key = os.getenv("STRIPE_SECRET_KEY", "")
receipt = {"configured": bool(key), "verified_test_mode": False}
if key:
    if not key.startswith(("sk_test_", "rk_test_")):
        raise SystemExit("Integration refused a Stripe credential without a test-mode prefix")
    for endpoint in ("account", "balance"):
        request = urllib.request.Request("https://api.stripe.com/v1/" + endpoint, headers={"Authorization": "Bearer " + key})
        try:
            with urllib.request.urlopen(request, timeout=20) as response:
                body = json.load(response)
        except urllib.error.HTTPError as error:
            raise SystemExit("Stripe test preflight rejected /v1/" + endpoint + " HTTP " + str(error.code)) from None
        if endpoint == "account":
            receipt["account_id"] = body["id"]
        elif body.get("livemode") is not False:
            raise SystemExit("Stripe test preflight did not confirm test-mode balance")
    receipt["verified_test_mode"] = True
path = Path("/var/lib/superserve-devbox/pricing-stripe-binding.json")
path.parent.mkdir(parents=True, exist_ok=True)
path.write_text(json.dumps(receipt) + "\n")
print(json.dumps(receipt))
