CREATE TABLE public."catalogs" (
  "id" uuid NOT NULL,
  "code" text NOT NULL,
  "created_time" timestamp with time zone NOT NULL DEFAULT now(),
  "updated_time" timestamp with time zone NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  CONSTRAINT "catalogs_pkey" PRIMARY KEY ("id")
);
CREATE UNIQUE INDEX "catalogs_code_key" ON public."catalogs" ("code");
CREATE TABLE public."marks" (
  "id" uuid NOT NULL,
  "note" text,
  "created_time" timestamp with time zone NOT NULL DEFAULT now(),
  "updated_time" timestamp with time zone NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  CONSTRAINT "marks_pkey" PRIMARY KEY ("id")
);
CREATE TABLE public."widgets" (
  "tenant_id" text NOT NULL DEFAULT current_setting('app.tenant'),
  "id" uuid NOT NULL,
  "name" character varying(64) NOT NULL,
  "kind" text NOT NULL DEFAULT 'small',
  "weight" double precision NOT NULL,
  "ratio" real,
  "count" integer NOT NULL DEFAULT 0,
  "total" bigint NOT NULL,
  "active" boolean NOT NULL DEFAULT true,
  "payload" jsonb,
  "blob" bytea,
  "seen" timestamp with time zone,
  "code" text NOT NULL,
  "serial" text NOT NULL,
  "a_long_field_name_that_pushes_the_check_name_past_the_limit" text,
  "created_time" timestamp with time zone NOT NULL DEFAULT now(),
  "updated_time" timestamp with time zone NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  "authz_token" text NOT NULL DEFAULT '',
  CONSTRAINT "widgets_pkey" PRIMARY KEY ("tenant_id", "id"),
  CONSTRAINT "widgets_name_check" CHECK (char_length("name") >= 2),
  CONSTRAINT "widgets_kind_check" CHECK ("kind" IN ('small', 'large')),
  CONSTRAINT "widgets_weight_check" CHECK ("weight" >= 0 AND "weight" <= 1000 AND "weight" NOT IN ('NaN'::double precision, 'Infinity'::double precision, '-Infinity'::double precision)),
  CONSTRAINT "widgets_ratio_check" CHECK ("ratio" >= 0 AND "ratio" <= 1 AND "ratio" NOT IN ('NaN'::double precision, 'Infinity'::double precision, '-Infinity'::double precision)),
  CONSTRAINT "widgets_total_check" CHECK ("total" >= 1),
  CONSTRAINT "widgets_a_long_field_name_that_pushes_the_chec_102c61d509823c93" CHECK (char_length("a_long_field_name_that_pushes_the_check_name_past_the_limit") >= 1)
);
CREATE UNIQUE INDEX "widgets_name_key" ON public."widgets" ("tenant_id", "name");
CREATE UNIQUE INDEX "widgets_serial_key" ON public."widgets" ("tenant_id", "serial");
CREATE UNIQUE INDEX "widgets_code_kind_key" ON public."widgets" ("tenant_id", "code", "kind");
CREATE UNIQUE INDEX "widgets_name_code_key" ON public."widgets" ("tenant_id", "name", "code");
CREATE TABLE public."orders" (
  "tenant_id" text NOT NULL DEFAULT current_setting('app.tenant'),
  "id" uuid NOT NULL,
  "widget_id" uuid NOT NULL,
  "catalog_id" uuid NOT NULL,
  "created_time" timestamp with time zone NOT NULL DEFAULT now(),
  "updated_time" timestamp with time zone NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  CONSTRAINT "orders_pkey" PRIMARY KEY ("tenant_id", "id"),
  CONSTRAINT "orders_widget_id_fkey" FOREIGN KEY ("tenant_id", "widget_id") REFERENCES public."widgets" ("tenant_id", "id"),
  CONSTRAINT "orders_catalog_id_fkey" FOREIGN KEY ("catalog_id") REFERENCES public."catalogs" ("id")
);
