# Using External PostGIS Database

This version of the Nominatim Docker image has been modified to work with an external PostGIS database instead of running PostgreSQL inside the container. This approach provides better separation of concerns and allows for more flexible deployment scenarios.

## Requirements

### External PostgreSQL Database

You need to provide a PostgreSQL database server (version 12 or later) with the following extensions installed:
- PostGIS (version 3.0 or later)
- hstore extension

### Database Configuration

Your PostgreSQL server should be configured with appropriate settings for Nominatim. Refer to the [official Nominatim documentation](https://nominatim.org/release-docs/5.3/admin/Installation/#tuning-the-postgresql-database) for recommended PostgreSQL configuration.

Key settings to consider:
- `shared_buffers`: At least 2GB (adjust based on available RAM)
- `maintenance_work_mem`: At least 10GB for imports
- `work_mem`: 50MB or higher
- `effective_cache_size`: 75% of available RAM
- `checkpoint_timeout`: 10min during imports

## Environment Variables

The following environment variables are used to configure the external database connection:

### Required Variables
- `POSTGRES_HOST`: Hostname or IP address of the PostgreSQL server (default: `postgres`)
- `POSTGRES_PORT`: Port number of the PostgreSQL server (default: `5432`)
- `POSTGRES_DB`: Name of the database to use (default: `nominatim`)
- `NOMINATIM_PASSWORD`: Password for the Nominatim database users
- `POSTGRES_ADMIN_PASSWORD`: Password for the PostgreSQL superuser. Required for the initial import, never derived from `NOMINATIM_PASSWORD`

### Optional Variables
All other Nominatim configuration variables remain the same as in the original documentation.


## Database Setup

### Automatic Setup
The Nominatim container will automatically:
1. Wait for the PostgreSQL server to be ready
2. Create the required database users (`nominatim` and `www-data`)
3. Set up the database with PostGIS and hstore extensions
4. Run the Nominatim import process

### Manual Setup (Optional)
If you prefer to set up the database manually, you can create the required users and database:

```sql
-- Connect as postgres superuser.
-- The application role needs CREATEDB, not SUPERUSER: installing PostGIS is the
-- only step that requires superuser, and the administrator does it once below.
CREATE USER nominatim CREATEDB;
CREATE USER "www-data";

-- Install the extensions into template1 so the database the nominatim role
-- creates inherits them. The container does this automatically unless you set
-- PROVISION_EXTENSIONS=false.
\c template1
CREATE EXTENSION IF NOT EXISTS postgis;
CREATE EXTENSION IF NOT EXISTS hstore;

-- Set passwords (replace with your actual password)
ALTER USER nominatim WITH ENCRYPTED PASSWORD 'your_password';
ALTER USER "www-data" WITH ENCRYPTED PASSWORD 'your_password';

-- Mark the roles as managed. Without this comment the container refuses to
-- touch roles it did not create (so it can never hijack a pre-existing
-- "www-data" on a shared cluster) and startup fails.
COMMENT ON ROLE nominatim IS 'managed by nominatim-docker';
COMMENT ON ROLE "www-data" IS 'managed by nominatim-docker';

-- Create database
CREATE DATABASE nominatim OWNER nominatim;

-- Connect to the nominatim database and enable extensions
\c nominatim
CREATE EXTENSION IF NOT EXISTS postgis;
CREATE EXTENSION IF NOT EXISTS hstore;
```

## Import Process

The import process works the same as before:

1. The container starts and waits for the external PostgreSQL server
2. It configures the database connection and creates necessary users/database
3. Downloads the specified PBF file (if `PBF_URL` is provided)
4. Runs the Nominatim import process
5. Starts the API server on port 8080

## Production Considerations

### Security
- Use strong passwords for database users
- Consider using connection pooling (e.g., PgBouncer)
- Restrict network access to the PostgreSQL server
- Use SSL connections in production environments

### Performance
- Use SSD storage for the PostgreSQL data directory
- Allocate sufficient RAM to the PostgreSQL server
- Consider using a dedicated server for the database
- Monitor and tune PostgreSQL configuration based on your workload

### Backup and Recovery
- Set up regular backups of the PostgreSQL database
- Consider using streaming replication for high availability
- Test your backup and recovery procedures

## Troubleshooting

### Connection Issues
If the Nominatim container cannot connect to PostgreSQL:

1. Verify that `POSTGRES_HOST` and `POSTGRES_PORT` are correct
2. Check that PostgreSQL is accepting connections from the Nominatim container
3. Verify that the password in `NOMINATIM_PASSWORD` is correct
4. Check PostgreSQL logs for connection errors

### Permission Issues
If you encounter permission errors:

1. Ensure the `nominatim` role has `CREATEDB`, and that PostGIS and hstore are installed in `template1`
   (or set `PROVISION_EXTENSIONS=false` and install them yourself). Set `NOMINATIM_ROLE_OPTIONS=SUPERUSER`
   only if your provider cannot pre-install extensions.
2. Verify that the database exists and is accessible
3. Check that PostGIS and hstore extensions are installed

### Performance Issues
If imports or queries are slow:

1. Review PostgreSQL configuration settings
2. Ensure adequate RAM and storage performance
3. Monitor PostgreSQL logs for slow queries
4. Consider adjusting Nominatim-specific settings

## Migration from Integrated PostgreSQL

If you're migrating from the integrated PostgreSQL version:

1. Dump your existing Nominatim database: `pg_dump -U nominatim nominatim > nominatim_backup.sql`
2. Set up the external PostgreSQL server with PostGIS
3. Update your Docker configuration to use the new image
4. Restore the database: `psql -U nominatim -h your_postgres_host nominatim < nominatim_backup.sql`
5. Update any scripts or automation to use the new environment variables