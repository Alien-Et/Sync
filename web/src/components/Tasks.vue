<template>
  <v-container>
    <v-row>
      <v-col>
        <v-card>
          <v-card-title>Pending Tasks</v-card-title>
          <v-data-table :headers="headers" :items="tasks" :items-per-page="10">
            <template v-slot:item.last_attempt="{ item }">
              {{ new Date(item.last_attempt * 1000).toLocaleString() }}
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
        { text: 'ID', value: 'id' },
        { text: 'Path', value: 'path' },
        { text: 'Operation', value: 'operation' },
        { text: 'Status', value: 'status' },
        { text: 'Retries', value: 'retries' },
        { text: 'Last Attempt', value: 'last_attempt' },
        { text: 'Chunk Offset', value: 'chunk_offset' },
      ],
      tasks: [],
    }
  },
  mounted() {
    this.fetchTasks()
  },
  methods: {
    async fetchTasks() {
      const res = await fetch('/api/tasks', {
        headers: { Authorization: `Bearer ${localStorage.getItem('token')}` },
      })
      this.tasks = await res.json()
    },
  },
}
</script>