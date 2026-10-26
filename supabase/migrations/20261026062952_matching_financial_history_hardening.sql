-- Retained rules, cap reservations and cash-attribution holds are financial history.
CREATE FUNCTION guard_billing_matching_history_removal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'matching financial history cannot be removed' USING ERRCODE = '55000';
END $$;
REVOKE ALL ON FUNCTION guard_billing_matching_history_removal() FROM PUBLIC;

DO $$ DECLARE t text; r text; BEGIN
    FOREACH t IN ARRAY ARRAY['billing_contract_matching_rule', 'billing_matching_credit', 'billing_matching_invoice_inspection'] LOOP
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', t);
        FOREACH r IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
            IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
                EXECUTE format('REVOKE ALL ON %I FROM %I', t, r);
            END IF;
        END LOOP;
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'service_role') THEN
            EXECUTE format('GRANT SELECT, INSERT, UPDATE ON %I TO service_role', t);
        END IF;
        EXECUTE format('CREATE TRIGGER matching_history_no_truncate BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_matching_history_removal()', t);
    END LOOP;
END $$;
CREATE TRIGGER matching_inspection_no_delete BEFORE DELETE ON billing_matching_invoice_inspection
    FOR EACH ROW EXECUTE FUNCTION guard_billing_matching_history_removal();
