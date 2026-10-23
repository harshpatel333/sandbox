-- The promotion a contract was sold with. Its invoices may carry this one
-- discount; the contract's fee and terms never change because of it.
CREATE TABLE billing_contract_promotion (
    contract_id uuid PRIMARY KEY REFERENCES billing_commercial_contract(id),
    team_id uuid NOT NULL,
    discount_id text NOT NULL CHECK (btrim(discount_id) <> ''),
    promotion_code text NOT NULL CHECK (btrim(promotion_code) <> ''),
    coupon_id text NOT NULL CHECK (btrim(coupon_id) <> ''),
    coupon_duration text NOT NULL CHECK (coupon_duration IN ('once', 'repeating', 'forever')),
    amount_off_cents bigint CHECK (amount_off_cents > 0),
    percent_off numeric CHECK (percent_off > 0 AND percent_off <= 100),
    checkout_invoice_id text NOT NULL UNIQUE CHECK (btrim(checkout_invoice_id) <> ''),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((amount_off_cents IS NULL) <> (percent_off IS NULL))
);

-- How much of one paid invoice's licensed fee the contract promotion covered.
-- Written in the transaction that records the invoice's base payment.
CREATE TABLE billing_contract_payment_discount (
    invoice_id text PRIMARY KEY CHECK (btrim(invoice_id) <> ''),
    contract_id uuid NOT NULL REFERENCES billing_contract_promotion(contract_id),
    discounted_cents bigint NOT NULL CHECK (discounted_cents > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION guard_billing_promotion_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'promotion records are immutable';
END $$;
CREATE TRIGGER billing_contract_promotion_guard BEFORE UPDATE OR DELETE ON billing_contract_promotion
    FOR EACH ROW EXECUTE FUNCTION guard_billing_promotion_immutable();
CREATE TRIGGER billing_contract_promotion_truncate_guard BEFORE TRUNCATE ON billing_contract_promotion
    FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_promotion_immutable();
CREATE TRIGGER billing_contract_payment_discount_guard BEFORE UPDATE OR DELETE ON billing_contract_payment_discount
    FOR EACH ROW EXECUTE FUNCTION guard_billing_promotion_immutable();
CREATE TRIGGER billing_contract_payment_discount_truncate_guard BEFORE TRUNCATE ON billing_contract_payment_discount
    FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_promotion_immutable();

-- A receipt's cash must cover the licensed fee less its recorded promotion
-- discount. This replaces the column check, which could not see the discount.
CREATE FUNCTION guard_billing_contract_payment_cash() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE discounted bigint;
BEGIN
    SELECT d.discounted_cents INTO discounted FROM billing_contract_payment_discount d
    WHERE d.invoice_id = NEW.invoice_id AND d.contract_id = NEW.contract_id;
    IF NEW.invoice_amount_paid_cents < NEW.base_amount_paid_cents - COALESCE(discounted, 0)
        OR COALESCE(discounted, 0) > NEW.base_amount_paid_cents THEN
        RAISE EXCEPTION 'invoice cash does not cover the licensed fee';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER billing_contract_payment_cash_guard BEFORE INSERT ON billing_contract_payment
    FOR EACH ROW EXECUTE FUNCTION guard_billing_contract_payment_cash();

DO $$ DECLARE c text; BEGIN
    SELECT conname INTO STRICT c FROM pg_constraint
    WHERE conrelid = 'billing_contract_payment'::regclass AND contype = 'c'
        AND pg_get_constraintdef(oid) = 'CHECK ((invoice_amount_paid_cents >= base_amount_paid_cents))';
    EXECUTE format('ALTER TABLE billing_contract_payment DROP CONSTRAINT %I', c);
END $$;

DO $$ DECLARE r text; t text; f regprocedure; BEGIN
    FOREACH t IN ARRAY ARRAY['billing_contract_promotion', 'billing_contract_payment_discount'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', t);
        FOREACH r IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
            IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
                EXECUTE format('REVOKE ALL ON %I FROM %I', t, r);
                IF r = 'service_role' THEN
                    EXECUTE format('GRANT SELECT, INSERT ON %I TO service_role', t);
                    EXECUTE format('CREATE POLICY %I ON %I TO service_role USING (true) WITH CHECK (true)', t || '_backend', t);
                END IF;
            END IF;
        END LOOP;
    END LOOP;
    FOREACH f IN ARRAY ARRAY['guard_billing_promotion_immutable()'::regprocedure, 'guard_billing_contract_payment_cash()'::regprocedure] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'service_role') THEN
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO service_role', f);
        END IF;
    END LOOP;
END $$;
