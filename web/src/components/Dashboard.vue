<template>
  <v-container>
    <v-row>
      <v-col>
        <v-card>
          <v-card-title>Synchronized Files</v-card-title>
          <v-data-table :headers="headers" :items="files" :items-per-page="10">
            <template v-slot:item.local_mtime="{ item }">
              {{ new Date(item.local_mtime * 1000).toLocaleString() }}
            </template>
            <template v-slot:item.remote_mtime="{ item }">
              {{ new Date(item.remote_mtime * 1000).toLocaleString() }}
            </template>
            <template v-slot:item.last_sync="{ item }">
              {{ new Date(item.last_sync * 1000).toLocaleString() }}
            </template>
          </v-data-table>
        </v-card>
      </v-col>
    </v-row>
  </v-container>
</template>

<script>
export default {
  data() {
    return {
      headers: [
        { text: 'Path', value: 'path' },
        { text: 'Local Hash', value: 'local_hash' },
        { text: 'Remote Hash', value: 'remote_hash' },
        { text: 'Local Modified', value: 'local_mtime' },
        { text: 'Remote Modified', value: 'remote_mtime' },
        { text: 'Last Sync', value: 'last_sync' },
        { text: 'Status', value: 'status' },
      ],
      files: [],
    }
  },
  mounted() {
    this.fetchFiles()
  },
  methods: {
    async fetchFiles() {
      const res = await fetch('/api/files', {
        headers: { Authorization: `Bearer ${localStorage.getItem('token')}` },
      })
      this.files = await res.json()
    },
  },
}
</script>