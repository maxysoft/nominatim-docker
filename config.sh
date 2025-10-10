CONFIG_FILE=${PROJECT_DIR}/.env


if [[ "$PBF_URL" = "" && "$PBF_PATH" = "" ]]  ||  [[ "$PBF_URL" != "" && "$PBF_PATH" != "" ]]; then
    echo "You need to specify either the PBF_URL or PBF_PATH environment variable"
    echo "docker run -e PBF_URL=https://download.geofabrik.de/europe/monaco-latest.osm.pbf ..."
    echo "docker run -e PBF_PATH=/nominatim/data/monaco-latest.osm.pbf ..."
    exit 1
fi

if [ "$REPLICATION_URL" != "" ]; then
    sed -i "s|__REPLICATION_URL__|$REPLICATION_URL|g" ${CONFIG_FILE}
fi

# Use the specified replication update and recheck interval values if either or both are numbers, or use the default values

reg_num='^[0-9]+$'
if [[ $REPLICATION_UPDATE_INTERVAL =~ $reg_num ]]; then
    if [ "$REPLICATION_URL" = "" ]; then
        echo "You need to specify the REPLICATION_URL variable in order to set a REPLICATION_UPDATE_INTERVAL"
        exit 1
    fi
    sed -i "s/NOMINATIM_REPLICATION_UPDATE_INTERVAL=86400/NOMINATIM_REPLICATION_UPDATE_INTERVAL=$REPLICATION_UPDATE_INTERVAL/g" ${CONFIG_FILE}
fi
if [[ $REPLICATION_RECHECK_INTERVAL =~ $reg_num ]]; then
    if [ "$REPLICATION_URL" = "" ]; then
        echo "You need to specify the REPLICATION_URL variable in order to set a REPLICATION_RECHECK_INTERVAL"
        exit 1
    fi
    sed -i "s/NOMINATIM_REPLICATION_RECHECK_INTERVAL=900/NOMINATIM_REPLICATION_RECHECK_INTERVAL=$REPLICATION_RECHECK_INTERVAL/g" ${CONFIG_FILE}
fi

# External PostgreSQL Database Configuration

# Set default values for external database connection
if [ -z "$POSTGRES_HOST" ]; then
    POSTGRES_HOST="postgres"
fi

if [ -z "$POSTGRES_PORT" ]; then
    POSTGRES_PORT="5432"
fi

if [ -z "$POSTGRES_DB" ]; then
    POSTGRES_DB="nominatim"
fi

# Configure the database connection string
sed -i "s|__POSTGRES_HOST__|${POSTGRES_HOST}|g" ${CONFIG_FILE}
sed -i "s|__POSTGRES_PORT__|${POSTGRES_PORT}|g" ${CONFIG_FILE}
sed -i "s|__POSTGRES_DB__|${POSTGRES_DB}|g" ${CONFIG_FILE}
sed -i "s|__NOMINATIM_PASSWORD__|${NOMINATIM_PASSWORD}|g" ${CONFIG_FILE}

# import style tuning

if [ ! -z "$IMPORT_STYLE" ]; then
  sed -i "s|__IMPORT_STYLE__|${IMPORT_STYLE}|g" ${CONFIG_FILE}
else
  sed -i "s|__IMPORT_STYLE__|full|g" ${CONFIG_FILE}
fi

# if flatnode directory was created by volume / mount, use flatnode files

if [ -d "${PROJECT_DIR}/flatnode" ]; then sed -i 's\^NOMINATIM_FLATNODE_FILE=$\NOMINATIM_FLATNODE_FILE="/nominatim/flatnode/flatnode.file"\g' ${CONFIG_FILE}; fi

# enable use of optional TIGER address data

if [ "$IMPORT_TIGER_ADDRESSES" = "true" ] || [ -f "$IMPORT_TIGER_ADDRESSES" ]; then
  echo NOMINATIM_USE_US_TIGER_DATA=yes >> ${CONFIG_FILE}
fi
