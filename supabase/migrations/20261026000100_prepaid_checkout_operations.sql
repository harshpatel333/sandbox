-- An operation is a purchase intent, independent of browser and provider replies.
CREATE TABLE billing_prepaid_checkout_operation (
    id uuid PRIMARY KEY,
    team_id uuid NOT NULL REFERENCES team(id) ON DELETE CASCADE,
    actor_id uuid NOT NULL,
    amount_cents bigint NOT NULL CHECK (amount_cents >= 500 AND amount_cents <= 100000000 AND amount_cents % 500 = 0),
    success_url text NOT NULL,
    cancel_url text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE billing_prepaid_checkout_operation ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_prepaid_checkout_operation FROM PUBLIC;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN
        GRANT SELECT, INSERT ON billing_prepaid_checkout_operation TO service_role;
    END IF;
END $$;
