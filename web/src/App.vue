<template>
  <v-app>
    <v-navigation-drawer v-model="drawer" app>
      <v-list>
        <v-list-item to="/">
          <v-list-item-title>Dashboard</v-list-item-title>
        </v-list-item>
        <v-list-item to="/config">
          <v-list-item-title>Configuration</v-list-item-title>
        </v-list-item>
        <v-list-item to="/tasks">
          <v-list-item-title>Tasks</v-list-item-title>
        </v-list-item>
        <v-list-item to="/conflicts">
          <v-list-item-title>Conflicts</v-list-item-title>
        </v-list-item>
        <v-list-item @click="logout">
          <v-list-item-title>Logout</v-list-item-title>
        </v-list-item>
      </v-list>
    </v-navigation-drawer>

    <v-app-bar app>
      <v-app-bar-nav-icon @click="drawer = !drawer"></v-app-bar-nav-icon>
      <v-toolbar-title>WebDAV Sync</v-toolbar-title>
      <v-spacer></v-spacer>
      <v-btn :disabled="status.paused" @click="pause">Pause</v-btn>
      <v-btn :disabled="!status.paused" @click="resume">Resume</v-btn>
      <v-chip :color="status.networkAvailable ? 'green' : 'red'">
        {{ status.networkAvailable ? 'Online' : 'Offline' }}
      </v-chip>
    </v-app-bar>

    <v-main>
      <v-container>
        <router-view></router-view>
      </v-container>
    </v-main>

    <v-footer app>
      <v-col>
        <v-card>
          <v-card-text>
            <div v-for="log in logs" :key="log.timestamp">{{ log.timestamp }}: {{ log.message }}</div>
          </v-card-text>
        </v-card>
      </v-col>
    </v-footer>
  </v-app>
</template>

<script>
export default {
  data() {
    return {
      drawer: false,
      status: { paused: false, networkAvailable: true },
      logs: [],
    }
  },
  mounted() {
    this.fetchStatus()
    this.connectLogs()
  },
  methods: {
    async fetchStatus() {
      const res = await fetch('/api/status', {
        headers: { Authorization: `Bearer ${localStorage.getItem('token')}` },
      })
      this.status = await res.json()
    },
    async pause() {
      await fetch('/api/pause', {
        method: 'POST',
        headers: { Authorization: `Bearer ${localStorage.getItem('token')}` },
      })
      this.status.paused = true
    },
    async resume() {
      await fetch('/api/resume', {
        method: 'POST',
        headers: { Authorization: `Bearer ${localStorage.getItem('token')}` },
      })
      this.status.paused = false
    },
    connectLogs() {
      const es = new EventSource('/api/logs')
      es.onmessage = (e) => {
        this.logs.push({ timestamp: new Date().toISOString(), message: e.data })
        if (this.logs.length > 100) this.logs.shift()
      }
      es.onerror = () => es.close()
    },
    logout() {
      localStorage.removeItem('token')
      this.$router.push('/login')
    },
  },
}
</script>