# cleanup the snapshot by replacing uuids with a placeholder so that the test is reproducible
sed -i '' 's/@flow_v1\/.\{36\}/<uuid>/g' ${SNAPSHOT}
