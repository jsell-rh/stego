ALTER TABLE public."widgets" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."widgets" FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS stego_tenant ON public."widgets";
CREATE POLICY stego_tenant ON public."widgets" USING ("tenant_id" = pg_catalog.current_setting('app.tenant', true)) WITH CHECK ("tenant_id" = pg_catalog.current_setting('app.tenant', true));
ALTER TABLE public."orders" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."orders" FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS stego_tenant ON public."orders";
CREATE POLICY stego_tenant ON public."orders" USING ("tenant_id" = pg_catalog.current_setting('app.tenant', true)) WITH CHECK ("tenant_id" = pg_catalog.current_setting('app.tenant', true));
