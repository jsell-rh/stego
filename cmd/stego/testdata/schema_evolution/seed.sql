-- The rows of ST-GEN-TD04, loaded after the migration job of v1: 10 tenants, each with one Organization, 1,000 User
-- rows each with its own email, and 10 OrgSetting rows with distinct keys. The check runs it as a superuser in one
-- transaction; the tables are owned and row-level security does not apply to a superuser.
BEGIN;
INSERT INTO public.stego_tenants (tenant_id)
  SELECT 't-' || pg_catalog.lpad(n::text, 2, '0') FROM pg_catalog.generate_series(0, 9) AS n;
INSERT INTO public.organizations (tenant_id, id, name)
  SELECT 't-' || pg_catalog.lpad(n::text, 2, '0'), ('00000000-0000-0000-0000-0000000000' || pg_catalog.lpad(n::text, 2, '0'))::uuid, 'org ' || n
  FROM pg_catalog.generate_series(0, 9) AS n;
INSERT INTO public.users (tenant_id, id, email, display_name, role, org_id)
  SELECT 't-' || pg_catalog.lpad((n / 1000)::text, 2, '0'), pg_catalog.gen_random_uuid(), 'user' || n || '@seed.test', 'User ' || n, 'member',
    ('00000000-0000-0000-0000-0000000000' || pg_catalog.lpad((n / 1000)::text, 2, '0'))::uuid
  FROM pg_catalog.generate_series(0, 9999) AS n;
INSERT INTO public.org_settings (tenant_id, id, org_id, key, value, generation)
  SELECT 't-' || pg_catalog.lpad((n / 10)::text, 2, '0'), pg_catalog.gen_random_uuid(),
    ('00000000-0000-0000-0000-0000000000' || pg_catalog.lpad((n / 10)::text, 2, '0'))::uuid, 'key-' || (n % 10), '{}'::jsonb, 1
  FROM pg_catalog.generate_series(0, 99) AS n;
COMMIT;
