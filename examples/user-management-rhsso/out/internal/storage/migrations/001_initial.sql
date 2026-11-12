CREATE TABLE public."organizations" (
  "tenant_id" text NOT NULL DEFAULT current_setting('app.tenant'),
  "id" uuid NOT NULL,
  "name" character varying(255) NOT NULL,
  "description" character varying(1000),
  "created_time" timestamp with time zone NOT NULL DEFAULT now(),
  "updated_time" timestamp with time zone NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  "authz_token" text NOT NULL DEFAULT '',
  CONSTRAINT "organizations_pkey" PRIMARY KEY ("tenant_id", "id"),
  CONSTRAINT "organizations_name_check" CHECK (char_length("name") >= 1)
);
CREATE INDEX "organizations_name_search" ON public."organizations" ("tenant_id", "name");
CREATE INDEX "organizations_description_search" ON public."organizations" ("tenant_id", "description");
CREATE UNIQUE INDEX "organizations_name_key" ON public."organizations" ("tenant_id", "name");
CREATE TABLE public."org_settings" (
  "tenant_id" text NOT NULL DEFAULT current_setting('app.tenant'),
  "id" uuid NOT NULL,
  "org_id" uuid NOT NULL,
  "key" character varying(255) NOT NULL,
  "value" jsonb NOT NULL,
  "generation" bigint NOT NULL,
  "created_time" timestamp with time zone NOT NULL DEFAULT now(),
  "updated_time" timestamp with time zone NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  "authz_token" text NOT NULL DEFAULT '',
  CONSTRAINT "org_settings_pkey" PRIMARY KEY ("tenant_id", "id"),
  CONSTRAINT "org_settings_org_id_fkey" FOREIGN KEY ("tenant_id", "org_id") REFERENCES public."organizations" ("tenant_id", "id")
);
CREATE INDEX "org_settings_org_id_search" ON public."org_settings" ("tenant_id", "org_id");
CREATE INDEX "org_settings_key_search" ON public."org_settings" ("tenant_id", "key");
CREATE INDEX "org_settings_generation_search" ON public."org_settings" ("tenant_id", "generation");
CREATE UNIQUE INDEX "org_settings_org_id_key_key" ON public."org_settings" ("tenant_id", "org_id", "key");
CREATE TABLE public."users" (
  "tenant_id" text NOT NULL DEFAULT current_setting('app.tenant'),
  "id" uuid NOT NULL,
  "email" character varying(255) NOT NULL,
  "display_name" character varying(100) NOT NULL,
  "role" text NOT NULL,
  "org_id" uuid NOT NULL,
  "metadata" jsonb,
  "created_time" timestamp with time zone NOT NULL DEFAULT now(),
  "updated_time" timestamp with time zone NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  "authz_token" text NOT NULL DEFAULT '',
  CONSTRAINT "users_pkey" PRIMARY KEY ("tenant_id", "id"),
  CONSTRAINT "users_display_name_check" CHECK (char_length("display_name") >= 1),
  CONSTRAINT "users_role_check" CHECK ("role" IN ('admin', 'member', 'viewer')),
  CONSTRAINT "users_org_id_fkey" FOREIGN KEY ("tenant_id", "org_id") REFERENCES public."organizations" ("tenant_id", "id")
);
CREATE INDEX "users_email_search" ON public."users" ("tenant_id", "email");
CREATE INDEX "users_display_name_search" ON public."users" ("tenant_id", "display_name");
CREATE INDEX "users_role_search" ON public."users" ("tenant_id", "role");
CREATE INDEX "users_org_id_search" ON public."users" ("tenant_id", "org_id");
CREATE UNIQUE INDEX "users_email_key" ON public."users" ("tenant_id", "email");
